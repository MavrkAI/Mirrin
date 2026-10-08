// Package googletest is a small stand-in for Google (sign-in, Gmail,
// Calendar, Drive, userinfo) for tests, so nothing ever leaves the machine.
// Point an Auth's Transport at Fake.Transport and every request to any
// Google host lands here instead.
package googletest

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/gmail/v1"
)

// Message is one Gmail message in the fake mailbox.
type Message struct {
	ID       string
	ThreadID string
	Labels   []string // e.g. INBOX, UNREAD
	Snippet  string
	Payload  *gmail.MessagePart
	// Received is when Gmail received it (internalDate, ms since 1970); 0
	// means after every message added before it.
	Received int64
	// GetStatus, when set, is how Google answers a request for this message
	// (404: deleted between being listed and being fetched).
	GetStatus int
}

// Event is one calendar event.
type Event struct {
	ID, Summary, Start, End string // RFC 3339
	// Free marks the event as not busy (a focus or working-location
	// block); Declined has the owner turn it down.
	Free, Declined bool
	Description    string
	HangoutLink    string // Meet's link
	VideoURI       string // another tool's video entry point
	Attendees      []Attendee
}

// Attendee is a guest on an Event.
type Attendee struct {
	Email, Name, Response string
	Self, Resource        bool
}

// Fake is the stand-in. Set its fields before (or between) calls; it is safe
// for concurrent use through its methods.
type Fake struct {
	Server *httptest.Server

	mu            sync.Mutex
	refreshes     int
	exchanges     int
	calls         map[string]int
	signedOut     bool            // refreshes answered invalid_grant (Testing mode's 7 days)
	clientDeleted bool            // everything answered deleted_client
	disabled      map[string]bool // API → turned off for the project
	unticked      map[string]bool // API → scope not granted
	grantScope    string          // the scope field of an exchange ("" = all)
	email         string
	messages      []Message
	events        []Event
	queries       []string // Gmail searches asked for, in order
	sent          [][]byte
	attachments   map[string][]byte // attachment id → bytes (long text parts Gmail keeps aside)
	access        string            // the access token currently valid
	serial        int
	outCode       string        // the error renewals get while signed out
	clock         int64         // the last Received given out
	holdRenewals  chan struct{} // while set, renewals wait until it is closed
	renewing      chan struct{} // told when a renewal starts waiting
}

// New starts a fake Google for the test.
func New(t testing.TB) *Fake {
	t.Helper()
	f := NewInProcess()
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// NewInProcess makes a fake that serves requests directly, without a socket.
// It uses the same handlers as New, including token and API failures.
func NewInProcess() *Fake {
	return &Fake{calls: map[string]int{}, disabled: map[string]bool{}, unticked: map[string]bool{}, email: "sam@example.com", access: "at-0"}
}

// Transport sends every request, whatever Google host it names, to the fake.
func (f *Fake) Transport() http.RoundTripper {
	if f.Server == nil {
		return roundTripper(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			f.serve(w, r.Clone(r.Context()))
			return w.Result(), nil
		})
	}
	target, _ := url.Parse(f.Server.URL)
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = target.Scheme, target.Host, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

// WriteFiles saves an OAuth client and a sign-in whose access token
// expires at expiry (in the past: the next call must renew it).
func (f *Fake) WriteFiles(t testing.TB, credentials, token string, expiry time.Time) {
	t.Helper()
	creds := `{"installed":{"client_id":"123-abc.apps.googleusercontent.com","client_secret":"GOCSPX-test","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token","redirect_uris":["http://localhost"]}}`
	if err := os.WriteFile(credentials, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.serial++
	access, refresh := f.access, fmt.Sprintf("rt-%d", f.serial) // each call is a new sign-in
	f.signedOut = false
	f.mu.Unlock()
	tok, _ := json.Marshal(map[string]any{"access_token": access, "token_type": "Bearer", "refresh_token": refresh, "expiry": expiry.Format(time.RFC3339)})
	if err := os.WriteFile(token, tok, 0o600); err != nil {
		t.Fatal(err)
	}
}

// SignOut makes Google refuse the sign-in, as it does 7 days after
// connecting while the project is in Testing.
func (f *Fake) SignOut() { f.SignOutWith("invalid_grant") }

// SignOutWith makes Google refuse renewals with the given error code
// (unauthorized_client: the sign-in belongs to a different client).
func (f *Fake) SignOutWith(code string) {
	f.mu.Lock()
	f.signedOut, f.outCode = true, code
	f.mu.Unlock()
}

// DeleteClient makes Google refuse the OAuth client itself.
func (f *Fake) DeleteClient() { f.mu.Lock(); f.clientDeleted = true; f.mu.Unlock() }

// NewClient stands for the owner making a new OAuth client after one was deleted.
func (f *Fake) NewClient() { f.mu.Lock(); f.clientDeleted = false; f.mu.Unlock() }

// Disable turns an API ("calendar", "gmail", "drive") off for the project.
func (f *Fake) Disable(api string) { f.mu.Lock(); f.disabled[api] = true; f.mu.Unlock() }

// Enable turns it back on.
func (f *Fake) Enable(api string) { f.mu.Lock(); delete(f.disabled, api); f.mu.Unlock() }

// Untick leaves an API's box unticked on the consent screen.
func (f *Fake) Untick(api string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unticked[api] = true
	scopes := []string{"https://www.googleapis.com/auth/userinfo.email"}
	for a, s := range map[string]string{"calendar": "https://www.googleapis.com/auth/calendar", "gmail": "https://www.googleapis.com/auth/gmail.modify", "drive": "https://www.googleapis.com/auth/drive.readonly"} {
		if !f.unticked[a] {
			scopes = append(scopes, s)
		}
	}
	f.grantScope = strings.Join(scopes, " ")
}

// AddMessage puts a message in the mailbox. Messages arrive in the order
// they are added unless Received says otherwise.
func (f *Fake) AddMessage(m Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.Received == 0 {
		m.Received = max(f.clock+1000, 1_790_000_000_000)
	}
	f.clock = max(f.clock, m.Received)
	f.messages = append(f.messages, m)
}

// MarkRead takes the UNREAD label off a message, as reading it in Gmail does.
func (f *Fake) MarkRead(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.messages {
		if m.ID == id {
			f.messages[i].Labels = slices.DeleteFunc(slices.Clone(m.Labels), func(l string) bool { return l == "UNREAD" })
		}
	}
}

// Delete removes a message, as emptying the trash does.
func (f *Fake) Delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = slices.DeleteFunc(f.messages, func(m Message) bool { return m.ID == id })
}

// HoldRenewals makes renewals of the sign-in wait (a slow network).
// started receives once for each renewal that has begun waiting; release
// lets them all finish.
func (f *Fake) HoldRenewals() (started <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hold, begun := make(chan struct{}), make(chan struct{}, 16)
	f.holdRenewals, f.renewing = hold, begun
	var once sync.Once
	return begun, func() {
		once.Do(func() {
			f.mu.Lock()
			f.holdRenewals = nil
			f.mu.Unlock()
			close(hold)
		})
	}
}

// AddAttachment stores the bytes Gmail serves for an attachment id.
func (f *Fake) AddAttachment(id string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachments == nil {
		f.attachments = map[string][]byte{}
	}
	f.attachments[id] = data
}

// AddEvent puts an event on the calendar. One with the same ID is replaced,
// as when an event is moved.
func (f *Fake) AddEvent(e Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.events {
		if f.events[i].ID == e.ID {
			f.events[i] = e
			return
		}
	}
	f.events = append(f.events, e)
}

// Queries lists the Gmail searches asked for so far, in order.
func (f *Fake) Queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

// Refreshes counts sign-in renewals Google was asked for.
func (f *Fake) Refreshes() int { f.mu.Lock(); defer f.mu.Unlock(); return f.refreshes }

// Calls counts requests to a path.
func (f *Fake) Calls(path string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[path] }

// Sent returns the raw messages sent through Gmail.
func (f *Fake) Sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sent...)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		f.mu.Lock()
		hold, begun := f.holdRenewals, f.renewing
		f.mu.Unlock()
		if hold != nil {
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				begun <- struct{}{}
				<-hold
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.URL.Path]++
	if r.URL.Path == "/token" {
		f.token(w, r)
		return
	}
	if f.clientDeleted {
		apiError(w, 401, "UNAUTHENTICATED", "Request had invalid authentication credentials.")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.access || f.signedOut {
		apiError(w, 401, "UNAUTHENTICATED", "Request had invalid authentication credentials.")
		return
	}
	api := ""
	switch {
	case strings.HasPrefix(r.URL.Path, "/gmail/"):
		api = "gmail"
	case strings.HasPrefix(r.URL.Path, "/calendar/"):
		api = "calendar"
	case strings.HasPrefix(r.URL.Path, "/drive/"):
		api = "drive"
	}
	if f.disabled[api] {
		name := map[string]string{"gmail": "gmail.googleapis.com", "calendar": "calendar-json.googleapis.com", "drive": "drive.googleapis.com"}[api]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		fmt.Fprintf(w, `{"error":{"code":403,"message":"%s API has not been used in project 123 before or it is disabled. Enable it by visiting https://console.developers.google.com/apis/api/%s/overview?project=123 then retry.","errors":[{"message":"disabled","domain":"usageLimits","reason":"accessNotConfigured"}],"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED"}]}}`, api, name)
		return
	}
	if f.unticked[api] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","errors":[{"message":"Insufficient Permission","domain":"global","reason":"insufficientPermissions"}],"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case p == "/oauth2/v2/userinfo":
		writeJSON(w, map[string]any{"email": f.email})
	case p == "/gmail/v1/users/me/profile":
		writeJSON(w, map[string]any{"emailAddress": f.email})
	case p == "/gmail/v1/users/me/messages" && r.Method == http.MethodGet:
		q := r.URL.Query().Get("q")
		f.queries = append(f.queries, q)
		limit, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
		if limit <= 0 {
			limit = 100 // Gmail's default page
		}
		byTime := slices.Clone(f.messages)
		slices.SortStableFunc(byTime, func(a, b Message) int { return cmp.Compare(b.Received, a.Received) }) // newest first, as Gmail lists
		var list []map[string]string
		for _, m := range byTime {
			if strings.Contains(q, "is:unread") && !has(m.Labels, "UNREAD") || strings.Contains(q, "in:inbox") && !has(m.Labels, "INBOX") {
				continue
			}
			if len(list) == limit {
				break
			}
			list = append(list, map[string]string{"id": m.ID, "threadId": m.ThreadID})
		}
		writeJSON(w, map[string]any{"messages": list, "resultSizeEstimate": len(list)})
	case p == "/gmail/v1/users/me/messages/send":
		var body struct{ Raw string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := base64.URLEncoding.DecodeString(body.Raw)
		f.sent = append(f.sent, raw)
		writeJSON(w, map[string]any{"id": fmt.Sprintf("sent-%d", len(f.sent))})
	case strings.HasPrefix(p, "/gmail/v1/users/me/messages/"):
		id := strings.TrimPrefix(p, "/gmail/v1/users/me/messages/")
		if rest, ok := strings.CutSuffix(id, "/modify"); ok {
			writeJSON(w, map[string]any{"id": rest})
			return
		}
		if _, att, ok := strings.Cut(id, "/attachments/"); ok {
			data := f.attachments[att]
			writeJSON(w, map[string]any{"data": base64.URLEncoding.EncodeToString(data), "size": len(data)})
			return
		}
		for _, m := range f.messages {
			if m.ID == id && m.GetStatus != 0 {
				apiError(w, m.GetStatus, http.StatusText(m.GetStatus), "the fake was told to fail this message")
				return
			}
			if m.ID == id {
				writeJSON(w, map[string]any{"id": m.ID, "threadId": m.ThreadID, "labelIds": m.Labels, "snippet": m.Snippet, "payload": m.Payload, "internalDate": strconv.FormatInt(m.Received, 10)})
				return
			}
		}
		apiError(w, 404, "NOT_FOUND", "Requested entity was not found.")
	case p == "/calendar/v3/users/me/calendarList":
		writeJSON(w, map[string]any{"items": []map[string]string{{"id": "primary"}}})
	case strings.HasPrefix(p, "/calendar/v3/calendars/") && strings.HasSuffix(p, "/events") && r.Method == http.MethodGet:
		var items []map[string]any
		for _, e := range f.events {
			item := map[string]any{"id": e.ID, "summary": e.Summary, "status": "confirmed", "start": map[string]string{"dateTime": e.Start}, "end": map[string]string{"dateTime": e.End}}
			if e.Free {
				item["transparency"] = "transparent"
			}
			if e.Description != "" {
				item["description"] = e.Description
			}
			if e.HangoutLink != "" {
				item["hangoutLink"] = e.HangoutLink
			}
			if e.VideoURI != "" {
				item["conferenceData"] = map[string]any{"entryPoints": []map[string]string{{"entryPointType": "phone", "uri": "tel:+1-555-0100"}, {"entryPointType": "video", "uri": e.VideoURI}}}
			}
			var guests []map[string]any
			for _, a := range e.Attendees {
				guests = append(guests, map[string]any{"email": a.Email, "displayName": a.Name, "self": a.Self, "resource": a.Resource, "responseStatus": a.Response})
			}
			if e.Declined {
				guests = append(guests, map[string]any{"email": f.email, "self": true, "responseStatus": "declined"})
			}
			if guests != nil {
				item["attendees"] = guests
			}
			items = append(items, item)
		}
		writeJSON(w, map[string]any{"items": items})
	case p == "/drive/v3/about":
		writeJSON(w, map[string]any{"user": map[string]string{"emailAddress": f.email}})
	default:
		apiError(w, 404, "NOT_FOUND", "no such path in the fake: "+p)
	}
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "application/json")
	if f.clientDeleted {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"deleted_client","error_description":"The OAuth client was deleted."}`)
		return
	}
	switch r.Form.Get("grant_type") {
	case "refresh_token":
		f.refreshes++
		if f.signedOut {
			w.WriteHeader(400)
			if f.outCode != "" && f.outCode != "invalid_grant" {
				fmt.Fprintf(w, `{"error":%q,"error_description":"Unauthorized"}`, f.outCode)
				return
			}
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
			return
		}
	case "authorization_code":
		f.exchanges++
		f.signedOut = false
	}
	f.serial++
	f.access = fmt.Sprintf("at-%d", f.serial)
	resp := map[string]any{"access_token": f.access, "token_type": "Bearer", "expires_in": 3599}
	if r.Form.Get("grant_type") == "authorization_code" {
		resp["refresh_token"] = fmt.Sprintf("rt-%d", f.serial+1)
		if f.grantScope != "" {
			resp["scope"] = f.grantScope
		}
	}
	writeJSON(w, resp)
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func apiError(w http.ResponseWriter, code int, status, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"status":%q}}`, code, msg, status)
}

// Part builds a Gmail message part: data is the part's raw bytes (Gmail
// sends them base64url-encoded, transfer encoding already undone, charset
// untouched).
func Part(mimeType, contentType, filename string, data []byte, size int64, parts ...*gmail.MessagePart) *gmail.MessagePart {
	p := &gmail.MessagePart{MimeType: mimeType, Filename: filename, Parts: parts, Body: &gmail.MessagePartBody{Size: size}}
	if contentType != "" {
		p.Headers = append(p.Headers, &gmail.MessagePartHeader{Name: "Content-Type", Value: contentType})
	}
	if filename != "" {
		p.Headers = append(p.Headers, &gmail.MessagePartHeader{Name: "Content-Disposition", Value: `attachment; filename="` + filename + `"`})
		p.Body.AttachmentId = "att-" + filename
	} else if data != nil {
		p.Body.Data = base64.URLEncoding.EncodeToString(data)
		p.Body.Size = int64(len(data))
	}
	return p
}

// Headers adds message headers (From, To, Subject, Date…) to a part.
func Headers(p *gmail.MessagePart, kv ...string) *gmail.MessagePart {
	for i := 0; i+1 < len(kv); i += 2 {
		p.Headers = append(p.Headers, &gmail.MessagePartHeader{Name: kv[i], Value: kv[i+1]})
	}
	return p
}
