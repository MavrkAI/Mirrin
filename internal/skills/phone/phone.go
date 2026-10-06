// Package phone gives the twin a voice on the telephone network through
// Twilio: text messages, and calls where it speaks and (when the daemon is
// reachable from the internet) listens and replies, turn by turn. This is the
// fallback for the parts of life that only answer the phone.
package phone

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Client talks to Twilio.
type Client struct {
	cfg  func() config.Phone
	http *http.Client
	base string // Twilio API base, overridable in tests

	// Turn runs one exchange of a two-way call: what the other party said in,
	// what the twin says back out. Empty reply or [HANGUP] ends the call.
	Turn func(ctx context.Context, callID, purpose, heard string) (string, error)
	// OnEnd, if set, hears about every two-way call once it is over, so the
	// owner can be told how it went. It runs on its own goroutine.
	OnEnd func(Ended)
	// PublicURL, if set, is the twin's public address while phone.public_url
	// is empty: the https address other devices reach it at now, or "" when
	// there is none. Twilio's webhooks and their signatures use it.
	PublicURL func() string

	mu    sync.Mutex
	calls map[string]*call // by local id; finished calls are kept for keepCalls
}

// keepCalls is how long a finished call's transcript stays readable.
const keepCalls = 24 * time.Hour

type call struct {
	ID      string
	SID     string // Twilio's call id, which is what the model is given
	ChatKey string // the conversation that placed the call
	To      string
	Purpose string
	Opening string
	Turns   int
	Started time.Time
	Ended   time.Time
	Status  string
	Log     []string
	// turning counts replies being worked out. The other party can hang up
	// mid-thought; the report waits for the last line to be logged.
	turning sync.WaitGroup
}

// Ended describes a two-way call that is over.
type Ended struct {
	ID, SID, To, Purpose, ChatKey string
	// Status is Twilio's final call status: completed, busy, no-answer, failed or canceled.
	Status     string
	Transcript []string
}

// New builds a client; cfg is read live.
func New(cfg func() config.Phone) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, base: "https://api.twilio.com", calls: map[string]*call{}}
}

// publicURL is where Twilio reaches the twin: phone.public_url, else the
// address reach serves now, else "" (calls are one-way).
func (c *Client) publicURL(p config.Phone) string {
	if p.PublicURL != "" {
		return strings.TrimRight(p.PublicURL, "/")
	}
	if c.PublicURL != nil {
		return strings.TrimRight(c.PublicURL(), "/")
	}
	return ""
}

func (c *Client) creds() (sid, token, from string, err error) {
	p := c.cfg()
	sid, from = p.AccountSID, p.From
	token = p.AuthToken
	if token == "" && p.AuthTokenEnv != "" {
		token = envOr(p.AuthTokenEnv)
	}
	if sid == "" || token == "" || from == "" {
		return "", "", "", errors.New("phone is not set up: phone.account_sid, phone.auth_token and phone.from (your Twilio number) are needed")
	}
	return sid, token, from, nil
}

func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	sid, token, _, err := c.creds()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/2010-04-01/Accounts/"+sid+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(sid, token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("twilio: %s", e.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// SendSMS sends a text message.
func (c *Client) SendSMS(ctx context.Context, to, text string) (string, error) {
	_, _, from, err := c.creds()
	if err != nil {
		return "", err
	}
	var res struct {
		SID string `json:"sid"`
	}
	if err := c.post(ctx, "/Messages.json", url.Values{"To": {to}, "From": {from}, "Body": {text}}, &res); err != nil {
		return "", err
	}
	return res.SID, nil
}

// Call places a call. With a public URL the call is two-way; otherwise the
// twin says its piece and hangs up.
func (c *Client) Call(ctx context.Context, to, opening, purpose string) (string, error) {
	return c.call(ctx, "", to, opening, purpose)
}

func (c *Client) call(ctx context.Context, chatKey, to, opening, purpose string) (string, error) {
	_, _, from, err := c.creds()
	if err != nil {
		return "", err
	}
	p := c.cfg()
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	form := url.Values{"To": {to}, "From": {from}}
	public := c.publicURL(p)
	twoWay := public != "" && c.Turn != nil
	if twoWay {
		c.mu.Lock()
		c.prune()
		c.calls[id] = &call{ID: id, ChatKey: chatKey, To: to, Purpose: purpose, Opening: opening, Started: time.Now()}
		c.mu.Unlock()
		form.Set("Url", public+"/phone/turn?call="+id)
		form.Set("StatusCallback", public+"/phone/status?call="+id)
	} else {
		form.Set("Twiml", twiml(p.Voice, opening, "", "", false))
	}
	var res struct {
		SID string `json:"sid"`
	}
	if err := c.post(ctx, "/Calls.json", form, &res); err != nil {
		if twoWay {
			c.mu.Lock()
			delete(c.calls, id)
			c.mu.Unlock()
		}
		return "", err
	}
	if twoWay {
		c.mu.Lock()
		if cl := c.calls[id]; cl != nil && cl.SID == "" {
			cl.SID = res.SID
		}
		c.mu.Unlock()
	}
	return res.SID, nil
}

// prune forgets finished calls older than keepCalls. c.mu must be held.
func (c *Client) prune() {
	for id, cl := range c.calls {
		if !cl.Ended.IsZero() && time.Since(cl.Ended) > keepCalls {
			delete(c.calls, id)
		}
	}
}

// find looks a call up by the local id or Twilio's SID, also when the id is
// quoted inside a longer string. c.mu must be held.
func (c *Client) find(id string) *call {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if cl := c.calls[id]; cl != nil {
		return cl
	}
	for _, cl := range c.calls {
		if cl.SID != "" && (cl.SID == id || strings.Contains(id, cl.SID)) {
			return cl
		}
	}
	return nil
}

// twiml renders a spoken line, optionally gathering speech back to action.
func twiml(voice, say, action, language string, gather bool) string {
	if voice == "" || language == "" {
		// The defaults for the owner's country, as a new config would have.
		d := config.Default().Phone
		if voice == "" {
			voice = d.Voice
		}
		if language == "" {
			language = d.Language
		}
	}
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><Response>`)
	if gather {
		fmt.Fprintf(&b, `<Gather input="speech" action="%s" method="POST" language="%s" speechTimeout="auto" actionOnEmptyResult="true">`, esc(action), language)
		if say != "" {
			fmt.Fprintf(&b, `<Say voice="%s">%s</Say>`, voice, esc(say))
		}
		b.WriteString(`</Gather>`)
		fmt.Fprintf(&b, `<Say voice="%s">Goodbye.</Say><Hangup/>`, voice)
	} else {
		if say != "" {
			fmt.Fprintf(&b, `<Say voice="%s">%s</Say>`, voice, esc(say))
		}
		b.WriteString(`<Hangup/>`)
	}
	b.WriteString(`</Response>`)
	return b.String()
}

// Validate checks Twilio's request signature.
func Validate(authToken, fullURL string, form url.Values, signature string) bool {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fullURL)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(form.Get(k))
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(b.String()))
	return hmac.Equal([]byte(base64.StdEncoding.EncodeToString(mac.Sum(nil))), []byte(signature))
}

// Webhook handles Twilio's requests for two-way calls: /phone/turn and /phone/status.
func (c *Client) Webhook(w http.ResponseWriter, r *http.Request) {
	p := c.cfg()
	_, token, _, err := c.creds()
	if err != nil {
		http.Error(w, "phone not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	full := c.publicURL(p) + r.URL.RequestURI()
	if !Validate(token, full, r.PostForm, r.Header.Get("X-Twilio-Signature")) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	id := r.URL.Query().Get("call")
	w.Header().Set("Content-Type", "text/xml")
	c.mu.Lock()
	cl := c.calls[id]
	if cl == nil {
		c.mu.Unlock()
		_, _ = io.WriteString(w, twiml(p.Voice, "Sorry, I've lost track of this call.", "", "", false))
		return
	}
	if sid := r.PostForm.Get("CallSid"); sid != "" && cl.SID == "" {
		cl.SID = sid
	}
	if strings.HasSuffix(r.URL.Path, "/status") {
		var ended *Ended
		if st := r.PostForm.Get("CallStatus"); cl.Ended.IsZero() && (st == "completed" || st == "failed" || st == "busy" || st == "no-answer" || st == "canceled") {
			// Keep the transcript: the model reads it with call_transcript,
			// and the owner is told how it went.
			cl.Ended, cl.Status = time.Now(), st
			ended = &Ended{ID: cl.ID, SID: cl.SID, To: cl.To, Purpose: cl.Purpose, ChatKey: cl.ChatKey, Status: st}
		}
		c.mu.Unlock()
		if ended != nil && c.OnEnd != nil {
			go func() {
				cl.turning.Wait() // no new turn starts once Ended is set
				c.mu.Lock()
				ended.Transcript = append([]string(nil), cl.Log...)
				c.mu.Unlock()
				c.OnEnd(*ended)
			}()
		}
		w.WriteHeader(204)
		return
	}
	action := c.publicURL(p) + "/phone/turn?call=" + id
	heard := strings.TrimSpace(r.PostForm.Get("SpeechResult"))
	if !cl.Ended.IsZero() {
		c.mu.Unlock()
		_, _ = io.WriteString(w, twiml(p.Voice, "", "", "", false))
		return
	}
	if cl.Turns == 0 && heard == "" {
		// The call was just answered: open with the prepared line and listen.
		cl.Turns++
		cl.Log = append(cl.Log, "twin: "+cl.Opening)
		opening := cl.Opening
		c.mu.Unlock()
		_, _ = io.WriteString(w, twiml(p.Voice, opening, action, p.Language, true))
		return
	}
	cl.Turns++
	if heard != "" {
		cl.Log = append(cl.Log, "them: "+heard)
	}
	turns, purpose := cl.Turns, cl.Purpose
	cl.turning.Add(1)
	c.mu.Unlock()
	defer cl.turning.Done()
	if turns > 12 {
		c.logLine(cl, "twin: Thanks for your time. Goodbye.")
		_, _ = io.WriteString(w, twiml(p.Voice, "Thanks for your time. Goodbye.", "", "", false))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	reply, err := c.Turn(ctx, id, purpose, heard)
	if err != nil || strings.TrimSpace(reply) == "" {
		c.logLine(cl, "twin: Sorry, something went wrong on my end. Goodbye. (no answer in time)")
		_, _ = io.WriteString(w, twiml(p.Voice, "Sorry, something went wrong on my end. Goodbye.", "", "", false))
		return
	}
	hang := strings.Contains(reply, "[HANGUP]")
	reply = strings.TrimSpace(strings.ReplaceAll(reply, "[HANGUP]", ""))
	c.logLine(cl, "twin: "+reply)
	if hang {
		_, _ = io.WriteString(w, twiml(p.Voice, reply, "", "", false))
		return
	}
	_, _ = io.WriteString(w, twiml(p.Voice, reply, action, p.Language, true))
}

func (c *Client) logLine(cl *call, line string) {
	c.mu.Lock()
	cl.Log = append(cl.Log, line)
	c.mu.Unlock()
}

// Transcript returns the log of a call in progress or finished in the last
// day, by local id or Twilio SID.
func (c *Client) Transcript(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl := c.find(id); cl != nil {
		return append([]string(nil), cl.Log...)
	}
	return nil
}

// Tools returns send_sms and phone_call.
func (c *Client) Tools() []tools.Tool {
	return []tools.Tool{
		tools.New("send_sms", "Send a text message to a phone number from the twin's own number. Say who you are on the user's behalf.",
			tools.Schema(map[string]tools.Prop{
				"to":   {Type: "string", Description: "E.164, e.g. +61400000000", Required: true},
				"text": {Type: "string", Required: true},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ To, Text string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				sid, err := c.SendSMS(ctx, in.To, in.Text)
				if err != nil {
					return "", err
				}
				return "sent (" + sid + ")", nil
			}),
		tools.New("phone_call",
			"Ring a phone number and speak on the user's behalf: book a table, ask a tradesperson for a quote, confirm an appointment, chase something. Give an opening line that says who you are calling for and why. If the twin is reachable from the internet (phone.public_url) the call is two-way and you get the conversation back; otherwise the line is spoken and the call ends.",
			tools.Schema(map[string]tools.Prop{
				"to":      {Type: "string", Description: "E.164 number", Required: true},
				"opening": {Type: "string", Description: "What to say when they answer, e.g. \"Hi, I'm calling on behalf of Akshay Kumar to book a table for two on Friday at seven.\"", Required: true},
				"purpose": {Type: "string", Description: "What you're trying to achieve and any facts you may need during the call (names, dates, budget, what to accept or decline)", Required: true},
			}), tools.RiskDangerous,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ To, Opening, Purpose string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				sid, err := c.call(ctx, call.ChatKey, in.To, in.Opening, in.Purpose)
				if err != nil {
					return "", err
				}
				if c.publicURL(c.cfg()) == "" || c.Turn == nil {
					return "call placed (" + sid + "); the line was spoken and the call ends after it. For a two-way conversation set phone.public_url.", nil
				}
				return "call placed (" + sid + "). It runs turn by turn in the background, and the user will be told how it went when it ends. To follow it meanwhile, use call_transcript with this id: " + sid, nil
			}),
		tools.New("call_transcript", "Read the transcript of a two-way call placed with phone_call.",
			tools.Schema(map[string]tools.Prop{"id": {Type: "string", Required: true}}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				c.mu.Lock()
				defer c.mu.Unlock()
				cl := c.find(in.ID)
				if cl == nil {
					return "no transcript for that call: it was one-way, ended more than a day ago, or the id is wrong", nil
				}
				state := fmt.Sprintf("in progress, %d turn(s) so far", cl.Turns)
				if !cl.Ended.IsZero() {
					state = "ended (" + cl.Status + ")"
				}
				if len(cl.Log) == 0 {
					return "Call to " + cl.To + ", " + state + ". Nothing said yet.", nil
				}
				return "Call to " + cl.To + ", " + state + ":\n" + strings.Join(cl.Log, "\n"), nil
			}),
	}
}
