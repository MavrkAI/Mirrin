// Package signal connects the twin to Signal through signal-cli
// (https://github.com/AsamK/signal-cli). Register or link a number once:
//
//	signal-cli -a +61400000001 register   (or `link -n mirrin` to pair with your phone)
//
// then set channels.signal.account to that number and owner to yours. When
// signal-cli is linked to your own account instead, account and owner are the
// same number and you talk to the twin in Note to Self. The channel spawns
// `signal-cli -a ACCOUNT -o json jsonRpc` and speaks JSON-RPC on its stdio;
// set channels.signal.http to use an already running `signal-cli daemon --http`
// instead.
package signal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Channel is the Signal transport.
type Channel struct {
	channels.Tracker
	backlog       channels.Backlog // tells the replay at connect from live messages
	bin, httpAddr string
	account       string
	owner         string
	replyToOthers bool
	log           *slog.Logger
	http          *http.Client

	mu      sync.Mutex
	stdin   io.Writer
	pending map[int64]chan rpcResult
	nextID  atomic.Int64
	sent    map[int64]time.Time // timestamps of our own messages, so their sync copies aren't taken as the owner's
}

type rpcResult struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// New builds the channel.
func New(bin, httpAddr, account, owner string, replyToOthers bool, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	if bin == "" {
		bin = "signal-cli"
	}
	return &Channel{bin: bin, httpAddr: strings.TrimRight(httpAddr, "/"), account: strings.TrimSpace(account),
		owner: strings.TrimSpace(owner), replyToOthers: replyToOthers, log: log,
		http: &http.Client{Timeout: 60 * time.Second}, pending: map[int64]chan rpcResult{}, sent: map[int64]time.Time{}}
}

func (c *Channel) Name() string { return "signal" }

// OwnerChatID is the owner's number.
func (c *Channel) OwnerChatID() string { return c.owner }

// Start runs signal-cli (or subscribes to the daemon) until ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.account == "" {
		return channels.Fatal(errors.New("signal account missing (channels.signal.account)"))
	}
	backoff := channels.Backoff{Min: time.Second, Max: 30 * time.Second}
	for ctx.Err() == nil {
		started := time.Now()
		var err error
		if c.httpAddr != "" {
			err = c.runHTTP(ctx, handler)
		} else {
			err = c.runProcess(ctx, handler)
		}
		if ctx.Err() != nil {
			return nil
		}
		if channels.IsFatal(err) {
			c.Down(err)
			return err
		}
		c.Retrying(err)
		c.log.Warn("signal", "err", err)
		if !backoff.Wait(ctx, started) {
			return nil
		}
	}
	return nil
}

// linked reports whether signal-cli is a device of the owner's own account.
func (c *Channel) linked() bool { return channels.MatchOwner(c.owner, c.account) }

// runProcess spawns signal-cli in JSON-RPC stdio mode.
func (c *Channel) runProcess(ctx context.Context, handler channels.Handler) error {
	if _, err := exec.LookPath(c.bin); err != nil {
		return channels.Fatal(fmt.Errorf("%s not found: install signal-cli (brew install signal-cli) and register the account", c.bin))
	}
	cmd := exec.CommandContext(ctx, c.bin, "-a", c.account, "-o", "json", "jsonRpc")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	c.mu.Lock()
	c.stdin = stdin
	c.mu.Unlock()
	c.Up()
	c.log.Info("signal connected", "account", c.account)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 36<<20) // room for a capped 25 MiB attachment, base64 encoded
	started := false
	for sc.Scan() {
		if !started {
			// signal-cli is talking to Signal only once it prints something;
			// the queue it drains comes after that, not at spawn.
			started = true
			c.backlog.Connected()
		}
		c.onLine(ctx, sc.Bytes(), handler)
	}
	c.mu.Lock()
	c.stdin = nil
	c.mu.Unlock()
	err = cmd.Wait()
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return exitError(c.account, lastLine(msg))
	}
	return fmt.Errorf("signal-cli exited: %v", err)
}

// exitError words signal-cli's last complaint; the ones only the owner can
// fix stop the channel instead of restarting signal-cli every few seconds.
func exitError(account, msg string) error {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "not registered"):
		return channels.Fatal(fmt.Errorf("%s isn't registered or linked in signal-cli yet: run signal-cli -a %s register (or link), then reconnect", account, account))
	case strings.Contains(low, "authorization failed"), strings.Contains(low, "deauthorized"):
		return channels.Fatal(fmt.Errorf("Signal no longer accepts this device for %s (was it unlinked?); link or register it again, then reconnect", account))
	}
	return fmt.Errorf("signal-cli exited: %s", msg)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// runHTTP subscribes to the daemon's SSE event stream.
func (c *Channel) runHTTP(ctx context.Context, handler channels.Handler) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.httpAddr+"/api/v1/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("signal daemon events: %s", resp.Status)
	}
	c.Up()
	c.backlog.Connected()
	c.log.Info("signal connected", "daemon", c.httpAddr)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte("data:")) {
			c.onLine(ctx, bytes.TrimSpace(line[5:]), handler)
		}
	}
	return errors.New("event stream closed")
}

type dataMessage struct {
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
	GroupInfo *struct {
		ID string `json:"groupId"`
	} `json:"groupInfo"`
	Attachments []signalAttachment `json:"attachments"`
}

type notification struct {
	Method string `json:"method"`
	ID     *int64 `json:"id"`
	rpcResult
	Params struct {
		Envelope struct {
			Source       string       `json:"source"`
			SourceNumber string       `json:"sourceNumber"`
			SourceName   string       `json:"sourceName"`
			Timestamp    int64        `json:"timestamp"`
			DataMessage  *dataMessage `json:"dataMessage"`
			// SyncMessage carries what the account's other devices sent.
			SyncMessage *struct {
				SentMessage *struct {
					dataMessage
					Destination       string `json:"destination"`
					DestinationNumber string `json:"destinationNumber"`
				} `json:"sentMessage"`
			} `json:"syncMessage"`
		} `json:"envelope"`
	} `json:"params"`
}

func (c *Channel) onLine(ctx context.Context, line []byte, handler channels.Handler) {
	var n notification
	if err := json.Unmarshal(line, &n); err != nil {
		return
	}
	if n.ID != nil { // a reply to one of our requests
		c.mu.Lock()
		ch := c.pending[*n.ID]
		delete(c.pending, *n.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- n.rpcResult
		}
		return
	}
	if n.Method != "receive" {
		return
	}
	env := n.Params.Envelope
	from := env.SourceNumber
	if from == "" {
		from = env.Source
	}
	dm := env.DataMessage
	isOwner := dm != nil && channels.MatchOwner(c.owner, from, env.Source)
	if sync := env.SyncMessage; dm == nil && sync != nil && sync.SentMessage != nil && c.linked() {
		// Linked to the owner's own account: what they write in Note to Self
		// is for the twin. What they send anyone else arrives here too, and
		// is their own conversation, not an instruction.
		sm := sync.SentMessage
		if !channels.MatchOwner(c.account, sm.DestinationNumber, sm.Destination) || c.ownEcho(sm.Timestamp) {
			return
		}
		dm, from, isOwner = &sm.dataMessage, c.account, true
	}
	if dm == nil || dm.GroupInfo != nil {
		return
	}
	text, kind := strings.TrimSpace(dm.Message), ""
	if len(dm.Attachments) > 0 {
		kind = channels.MediaKind(dm.Attachments[0].ContentType)
		if dm.Attachments[0].VoiceNote {
			kind = channels.Voice
		}
	}
	if text == "" && kind == "" {
		return // receipts, typing, reactions
	}
	ts := env.Timestamp
	if dm.Timestamp > 0 {
		ts = dm.Timestamp
	}
	if ts > 0 && c.backlog.Stale(time.UnixMilli(ts)) {
		return
	}
	if !isOwner && !c.replyToOthers {
		return
	}
	sender := env.SourceName
	if sender == "" {
		sender = from
	}
	handler(ctx, channels.Inbound{Channel: "signal", ChatID: from, Sender: sender, Text: text, Media: kind, Attachment: c.attachment(dm.Attachments, from), IsOwner: isOwner})
}

// ownEcho reports whether a Note to Self message is one the twin sent.
func (c *Channel) ownEcho(ts int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.sent[ts]
	return ok
}

// remember notes the timestamp of a message the twin sent.
func (c *Channel) remember(res json.RawMessage) {
	var r struct {
		Timestamp int64 `json:"timestamp"`
	}
	if json.Unmarshal(res, &r) != nil || r.Timestamp == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for ts, at := range c.sent {
		if time.Since(at) > 10*time.Minute {
			delete(c.sent, ts)
		}
	}
	c.sent[r.Timestamp] = time.Now()
}

// rpc sends a JSON-RPC request through whichever transport is active.
func (c *Channel) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if c.httpAddr != "" {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpAddr+"/api/v1/rpc", bytes.NewReader(req))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(hreq)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var res rpcResult
		data, err := channels.ReadCapped(resp.Body, 36<<20)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("signal RPC: HTTP %d", resp.StatusCode)
		}
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, err
		}
		if res.Error != nil {
			if method == "getAttachment" && res.Error.Code == -32601 {
				return nil, ErrAttachmentUpgrade
			}
			return nil, fmt.Errorf("signal %s: %s", method, res.Error.Message)
		}
		return res.Result, nil
	}
	ch := make(chan rpcResult, 1)
	c.mu.Lock()
	w := c.stdin
	if w == nil {
		c.mu.Unlock()
		return nil, errors.New("signal-cli not running")
	}
	c.pending[id] = ch
	_, err := w.Write(append(req, '\n'))
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		if res.Error != nil {
			if method == "getAttachment" && res.Error.Code == -32601 {
				return nil, ErrAttachmentUpgrade
			}
			return nil, fmt.Errorf("signal %s: %s", method, res.Error.Message)
		}
		return res.Result, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("signal %s: timeout", method)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Send delivers text to a number (to the account's own number: Note to Self).
func (c *Channel) Send(ctx context.Context, chatID, text string) error {
	return channels.SendParts(text, 4000, func(part string) error {
		res, err := c.rpc(ctx, "send", map[string]any{"account": c.account, "recipient": []string{chatID}, "message": part})
		if err == nil {
			c.remember(res)
		}
		return err
	})
}

// Typing shows "typing…" to the other person; Signal clears it after fifteen
// seconds. There is no one to show it to in Note to Self.
func (c *Channel) Typing(ctx context.Context, chatID string) (time.Duration, error) {
	if channels.MatchOwner(c.account, chatID) {
		return 0, nil
	}
	_, err := c.rpc(ctx, "sendTyping", map[string]any{"account": c.account, "recipient": []string{chatID}})
	return 10 * time.Second, err
}

// SendImage sends a file attachment with a caption.
func (c *Channel) SendImage(ctx context.Context, chatID, path, caption string) error {
	res, err := c.rpc(ctx, "send", map[string]any{"account": c.account, "recipient": []string{chatID}, "message": caption, "attachments": []string{path}})
	c.remember(res)
	return err
}
