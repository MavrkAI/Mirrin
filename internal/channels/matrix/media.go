package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

var errMediaUnrecognized = errors.New("authenticated media endpoint not supported")

// download keeps credentials on the original origin, including on redirects.
// The hard limit also applies when a caller has not supplied a smaller cap.
func (c *Channel) download(ctx context.Context, address string, size, max int64) ([]byte, error) {
	if max <= 0 || max > 25<<20 {
		max = 25 << 20
	}
	if size > max {
		return nil, channels.ErrTooBig
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	client := *c.http
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many media redirects")
		}
		if next.URL.Scheme != req.URL.Scheme || next.URL.Host != req.URL.Host {
			next.Header.Del("Authorization")
			if next.URL.Scheme != "https" {
				return fmt.Errorf("unsafe media redirect")
			}
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		data, err := channels.ReadCapped(resp.Body, 4096)
		var result struct {
			Code string `json:"errcode"`
		}
		if err == nil && json.Unmarshal(data, &result) == nil && result.Code == "M_UNRECOGNIZED" {
			return nil, errMediaUnrecognized
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("matrix file: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, channels.ErrTooBig
	}
	return channels.ReadCapped(resp.Body, max)
}

// Media is fetched through our homeserver, never directly from an mxc host.
// https://spec.matrix.org/v1.13/client-server-api/#get_matrixclientv1mediadownloadservernamemediaid
type mediaContent struct {
	MsgType  string `json:"msgtype"`
	Body     string `json:"body"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
	Info     struct {
		Mime     string `json:"mimetype"`
		Size     int64  `json:"size"`
		Duration int    `json:"duration"` // milliseconds
	} `json:"info"`
}

func (c *Channel) attachment(f mediaContent) *channels.Attachment {
	if f.URL == "" || f.MsgType == "m.text" {
		return nil
	}
	return &channels.Attachment{Mime: f.Info.Mime, Size: f.Info.Size, Seconds: (f.Info.Duration + 999) / 1000, Fetch: func(ctx context.Context, max int64) ([]byte, error) {
		u, err := url.Parse(f.URL)
		if err != nil || u.Scheme != "mxc" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Count(u.Path, "/") != 1 || len(u.Path) < 2 || u.Path == "/.." || u.Path == "/." {
			return nil, fmt.Errorf("invalid Matrix media URI")
		}
		address := c.hs + "/_matrix/client/v1/media/download/" + url.PathEscape(u.Host) + "/" + url.PathEscape(strings.TrimPrefix(u.Path, "/"))
		data, err := c.download(ctx, address, f.Info.Size, max)
		if errors.Is(err, errMediaUnrecognized) {
			legacy := strings.Replace(address, "/_matrix/client/v1/media/download/", "/_matrix/media/v3/download/", 1)
			return c.download(ctx, legacy, f.Info.Size, max)
		}
		return data, err
	}}
}
