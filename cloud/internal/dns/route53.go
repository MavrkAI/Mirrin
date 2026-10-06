package dns

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/sigv4"
)

// Route53 writes records in one Route 53 hosted zone through its REST API
// (version 2013-04-01), signing with internal/sigv4.
type Route53 struct {
	// Zone is the tenant zone, such as "mirrin.link".
	Zone string
	// HostedZoneID is the zone's id, such as "Z0123456789ABCDEFGHIJ".
	HostedZoneID string
	// Creds sign the calls. The IAM policy should allow only
	// route53:ChangeResourceRecordSets and route53:ListResourceRecordSets on
	// this zone.
	Creds sigv4.Creds
	// Endpoint is https://route53.amazonaws.com unless a test says otherwise.
	Endpoint string
	// TTL for every record; 300 if zero.
	TTL int
	// HTTP sends the calls; nil means a client with a 20 s timeout.
	HTTP *http.Client
	// Now is the signing clock; nil means time.Now.
	Now func() time.Time
}

const (
	route53NS       = "https://route53.amazonaws.com/doc/2013-04-01/"
	route53Endpoint = "https://route53.amazonaws.com"
	// Route 53 is global; its signing region is us-east-1.
	route53Region  = "us-east-1"
	route53Service = "route53"
	maxRoute53     = 1 << 20
)

// The request and response shapes, in the element order of the Route 53
// API model.
type changeRequest struct {
	XMLName     xml.Name    `xml:"ChangeResourceRecordSetsRequest"`
	XMLNS       string      `xml:"xmlns,attr"`
	ChangeBatch changeBatch `xml:"ChangeBatch"`
}

type changeBatch struct {
	Comment string   `xml:"Comment,omitempty"`
	Changes []change `xml:"Changes>Change"`
}

type change struct {
	Action string `xml:"Action"`
	Set    rrset  `xml:"ResourceRecordSet"`
}

type rrset struct {
	Name    string   `xml:"Name"`
	Type    string   `xml:"Type"`
	TTL     int      `xml:"TTL"`
	Records []record `xml:"ResourceRecords>ResourceRecord"`
}

type record struct {
	Value string `xml:"Value"`
}

type listResponse struct {
	Sets        []rrset `xml:"ResourceRecordSets>ResourceRecordSet"`
	IsTruncated bool    `xml:"IsTruncated"`
}

func (r *Route53) fqdn(handle string) string { return handle + "." + r.Zone + "." }

func (r *Route53) ttl() int {
	if r.TTL > 0 {
		return r.TTL
	}
	return 300
}

func (r *Route53) check(handle string) error {
	if err := Label(handle); err != nil {
		return err
	}
	if r.HostedZoneID == "" || strings.Trim(r.HostedZoneID, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" || len(r.HostedZoneID) > 32 {
		return fmt.Errorf("dns: hosted zone id %q", r.HostedZoneID)
	}
	for l := range strings.SplitSeq(r.Zone, ".") {
		if err := Label(l); err != nil {
			return fmt.Errorf("dns: zone %q: %w", r.Zone, err)
		}
	}
	return nil
}

// UpsertRequest is the ChangeResourceRecordSets body UpsertHandle sends.
func (r *Route53) UpsertRequest(handle string, v4, v6 []netip.Addr, caa []CAA) ([]byte, error) {
	if err := r.check(handle); err != nil {
		return nil, err
	}
	if err := checkAddrs(v4, v6); err != nil {
		return nil, err
	}
	if len(caa) == 0 {
		return nil, errors.New("dns: a handle always has a CAA set")
	}
	name := r.fqdn(handle)
	var changes []change
	for _, s := range []struct {
		typ   string
		addrs []netip.Addr
	}{{"A", v4}, {"AAAA", v6}} {
		if len(s.addrs) == 0 {
			continue
		}
		set := rrset{Name: name, Type: s.typ, TTL: r.ttl()}
		for _, a := range s.addrs {
			set.Records = append(set.Records, record{a.String()})
		}
		changes = append(changes, change{Action: "UPSERT", Set: set})
	}
	set := rrset{Name: name, Type: "CAA", TTL: r.ttl()}
	for _, c := range caa {
		if !slices.Contains([]string{"issue", "issuewild", "iodef"}, c.Tag) || strings.ContainsAny(c.Value, "\"\\") {
			return nil, fmt.Errorf("dns: bad CAA record %s", c)
		}
		set.Records = append(set.Records, record{c.String()})
	}
	changes = append(changes, change{Action: "UPSERT", Set: set})
	return marshalChange(changeBatch{Comment: "mirrin-cloud: " + handle, Changes: changes})
}

func marshalChange(b changeBatch) ([]byte, error) {
	out, err := xml.MarshalIndent(changeRequest{XMLNS: route53NS, ChangeBatch: b}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}

// UpsertHandle implements Provider.
func (r *Route53) UpsertHandle(ctx context.Context, handle string, v4, v6 []netip.Addr, caa []CAA) error {
	body, err := r.UpsertRequest(handle, v4, v6, caa)
	if err != nil {
		return err
	}
	_, err = r.do(ctx, http.MethodPost, "/rrset", "", body)
	return err
}

// DeleteHandle implements Provider: it lists the handle's A, AAAA and CAA
// sets and deletes them exactly as listed, as Route 53 requires.
func (r *Route53) DeleteHandle(ctx context.Context, handle string) error {
	if err := r.check(handle); err != nil {
		return err
	}
	name := r.fqdn(handle)
	b, err := r.do(ctx, http.MethodGet, "/rrset", url.Values{"name": {name}, "maxitems": {"10"}}.Encode(), nil)
	if err != nil {
		return err
	}
	var l listResponse
	if err := xml.Unmarshal(b, &l); err != nil {
		return fmt.Errorf("dns: route53 list: %w", err)
	}
	var changes []change
	for _, s := range l.Sets {
		if strings.EqualFold(s.Name, name) && slices.Contains([]string{"A", "AAAA", "CAA"}, s.Type) {
			changes = append(changes, change{Action: "DELETE", Set: s})
		}
	}
	if len(changes) == 0 {
		return nil
	}
	body, err := marshalChange(changeBatch{Comment: "mirrin-cloud: release " + handle, Changes: changes})
	if err != nil {
		return err
	}
	_, err = r.do(ctx, http.MethodPost, "/rrset", "", body)
	return err
}

// do sends one signed call to the hosted zone's path and returns the body
// of a 2xx answer.
func (r *Route53) do(ctx context.Context, method, path, query string, body []byte) ([]byte, error) {
	if r.Creds.AccessKeyID == "" || r.Creds.SecretAccessKey == "" {
		return nil, errors.New("dns: no Route 53 credentials")
	}
	ep := r.Endpoint
	if ep == "" {
		ep = route53Endpoint
	}
	u := strings.TrimSuffix(ep, "/") + "/2013-04-01/hostedzone/" + r.HostedZoneID + path
	if query != "" {
		u += "?" + query
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "text/xml")
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	hash := sigv4.EmptyPayloadHash
	if body != nil {
		hash = sigv4.PayloadHash(body)
	}
	if err := sigv4.Sign(req, r.Creds, route53Region, route53Service, now(), hash); err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 20 * time.Second}
	if r.HTTP != nil {
		cp := *r.HTTP
		c = &cp
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dns: route53 %s: %w", method, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRoute53+1))
	if err != nil {
		return nil, fmt.Errorf("dns: route53 %s: %w", method, err)
	}
	if len(b) > maxRoute53 {
		return nil, fmt.Errorf("dns: route53 %s: answer over %d bytes", method, maxRoute53)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("dns: route53 %s: HTTP %d: %s", method, resp.StatusCode, errorText(b))
	}
	return b, nil
}

// errorText pulls the code and message out of a Route 53 error body.
func errorText(b []byte) string {
	var e struct {
		Code     string   `xml:"Error>Code"`
		Message  string   `xml:"Error>Message"`
		Messages []string `xml:"Messages>Message"`
	}
	if xml.Unmarshal(b, &e) != nil {
		return "unreadable error"
	}
	msg := strings.TrimSpace(e.Code + " " + e.Message + " " + strings.Join(e.Messages, "; "))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, msg)
}
