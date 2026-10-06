// Package irc puts the twin on an IRC server. It answers private messages
// from the owner and, in joined channels, lines addressed "nick: …". SASL
// PLAIN is used when a password is set (register the nick with NickServ first).
//
// Anyone can take any free nick, so a nick alone never makes someone the
// owner. The owner is recognised by their services account, which the
// server attaches to each message (IRCv3 account-tag), or by a full
// nick!user@host mask when the network has no services. Their conversation
// is keyed by that identity, not by whatever nick they are using, so no one
// who later takes one of their nicks can land in it.
package irc

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Channel is the IRC transport.
type Channel struct {
	server        string
	useTLS        bool
	nick          string
	password      string
	owner         string
	replyToOthers bool
	rooms         []string
	log           *slog.Logger

	mu         sync.Mutex
	conn       net.Conn
	w          *bufio.Writer
	last       time.Time
	ownerNick  string // the nick the owner was last verified under this session
	noAccounts bool   // the server said it won't tag messages with accounts
	widened    string // an old prefix mask, as configured, before "*" was added

	// Read-loop state, reset per connection.
	offered     []string // capabilities from CAP LS
	accountTags bool     // the server tags messages with the sender's account
}

// New builds the channel. owner is a services account name (usually the
// registered nick) or a nick!user@host mask; * and ? work in a mask.
func New(server string, useTLS bool, nick, password, owner string, replyToOthers bool, rooms []string, log *slog.Logger) *Channel {
	if log == nil {
		log = slog.Default()
	}
	if nick == "" {
		nick = "mirrin"
	}
	owner = strings.TrimSpace(owner)
	if strings.HasPrefix(owner, "@") && !strings.ContainsAny(owner[1:], "!@") {
		owner = owner[1:]
	}
	c := &Channel{server: server, useTLS: useTLS, nick: nick, password: password, owner: owner,
		replyToOthers: replyToOthers, rooms: rooms, log: log}
	// Masks used to match as prefixes. One that plainly stops short
	// ("akshay!~a@") keeps meaning what it did.
	if c.isMask() && !strings.ContainsAny(owner, "*?") && strings.ContainsAny(owner[len(owner)-1:], "!@./") {
		c.widened, c.owner = owner, owner+"*"
	}
	return c
}

func (c *Channel) Name() string { return "irc" }

// OwnerChatID is the owner's conversation, once they have spoken with their
// identity verified this session: Send delivers it to the nick they spoke
// from, so proactive messages never go to someone else wearing the owner's
// nick. It is empty until then.
func (c *Channel) OwnerChatID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownerNick == "" {
		return ""
	}
	return c.ownerID()
}

func (c *Channel) isMask() bool { return strings.ContainsAny(c.owner, "!@") }

// ownerID keys the owner's conversation, whatever nick they use: the
// account name, the nick a mask names, or the mask itself when its nick is
// a wildcard (a mask can never be someone's nick).
func (c *Channel) ownerID() string {
	if !c.isMask() {
		return c.owner
	}
	if nick, _, ok := strings.Cut(c.owner, "!"); ok && nick != "" && !strings.ContainsAny(nick, "*?") {
		return nick
	}
	return c.owner
}

// Warning is what the owner should fix, for the Channels page ("" when all
// is well).
func (c *Channel) Warning() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.isMask() && c.noAccounts:
		return "This IRC server doesn't say which account sent a message, so you can't be recognised by your nick alone. Set your IRC owner to your full nick!user@host."
	case c.anyHost():
		return fmt.Sprintf("Your IRC owner %q matches anyone who takes that nick. Use your NickServ account, or a full nick!user@host.", c.owner)
	}
	return ""
}

// anyHost reports whether the owner mask accepts any host, so a nick and a
// username, which anyone can pick, are all it takes to match.
func (c *Channel) anyHost() bool {
	if !c.isMask() {
		return false
	}
	host := c.owner
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	} else if _, rest, ok := strings.Cut(host, "!"); ok {
		host = rest
	}
	return strings.Trim(host, "*?") == ""
}

// Start connects and reads lines until ctx ends. Messages are handled one
// at a time off the read loop, so a long turn never stops the connection
// answering the server's PINGs.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	if c.server == "" {
		return channels.Fatal(errors.New("irc server missing"))
	}
	if c.widened != "" {
		c.log.Warn("irc: your owner mask used to match as a prefix; it now reads "+c.owner+". A full nick!user@host, or your NickServ account, is safer.", "was", c.widened)
	}
	box := newInbox(32)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			in, ok := box.next(done)
			if !ok {
				return
			}
			handler(ctx, in)
		}
	}()
	enqueue := func(_ context.Context, in channels.Inbound) {
		if !box.put(in) {
			c.log.Warn("irc: too many messages waiting; dropped one", "from", in.Sender, "owner", in.IsOwner)
		}
	}
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		err := c.session(ctx, enqueue)
		if ctx.Err() != nil {
			return nil
		}
		if channels.IsFatal(err) {
			return err
		}
		c.log.Warn("irc", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
	return nil
}

func (c *Channel) raw(format string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w == nil {
		return errors.New("irc not connected")
	}
	// A CR or LF inside a line would start a new command on the server.
	line := strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(fmt.Sprintf(format, args...))
	line = cut(line, 500)
	if _, err := c.w.WriteString(line + "\r\n"); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *Channel) session(ctx context.Context, handler channels.Handler) error {
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if c.useTLS {
		host, _, _ := net.SplitHostPort(c.server)
		conn, err = tls.DialWithDialer(&d, "tcp", c.server, &tls.Config{ServerName: host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", c.server)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()
	c.mu.Lock()
	c.conn, c.w = conn, bufio.NewWriter(conn)
	c.ownerNick, c.noAccounts = "", false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn, c.w = nil, nil
		c.mu.Unlock()
	}()
	c.offered, c.accountTags = nil, false
	// Ask what the server offers; registration waits for CAP END. Servers
	// without capability negotiation ignore this and carry on.
	_ = c.raw("CAP LS 302")
	_ = c.raw("NICK %s", c.nick)
	_ = c.raw("USER %s 0 * :Mirrin", c.nick)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if err := c.onLine(ctx, line, handler); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("connection closed: %v", sc.Err())
}

// parse splits "@tags :prefix COMMAND params :trailing" (tags are skipped;
// see parseTags).
func parse(line string) (prefix, cmd string, params []string) {
	if strings.HasPrefix(line, "@") {
		if i := strings.Index(line, " "); i > 0 {
			line = line[i+1:]
		}
	}
	if strings.HasPrefix(line, ":") {
		if i := strings.Index(line, " "); i > 0 {
			prefix, line = line[1:i], line[i+1:]
		}
	}
	trailing := ""
	if i := strings.Index(line, " :"); i >= 0 {
		trailing, line = line[i+2:], line[:i]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return prefix, "", nil
	}
	cmd, params = strings.ToUpper(fields[0]), fields[1:]
	if trailing != "" || strings.HasSuffix(line, " :") {
		params = append(params, trailing)
	}
	return
}

// parseTags reads IRCv3 message tags ("@account=akshay;time=…").
func parseTags(line string) map[string]string {
	if !strings.HasPrefix(line, "@") {
		return nil
	}
	raw, _, _ := strings.Cut(line[1:], " ")
	tags := map[string]string{}
	unescape := strings.NewReplacer(`\:`, ";", `\s`, " ", `\\`, `\`, `\r`, "\r", `\n`, "\n")
	for _, t := range strings.Split(raw, ";") {
		k, v, _ := strings.Cut(t, "=")
		if k != "" {
			tags[k] = unescape.Replace(v)
		}
	}
	return tags
}

func nickOf(prefix string) string {
	if i := strings.Index(prefix, "!"); i > 0 {
		return prefix[:i]
	}
	return prefix
}

// sender decides who a message is from. owner means verified; impostor
// means someone unverified whose nick is the owner's conversation key: they
// would land in the owner's conversation, with its history and approvals.
func (c *Channel) sender(prefix string, tags map[string]string) (owner, impostor bool) {
	if c.isMask() {
		target := prefix
		if !strings.Contains(c.owner, "!") { // "user@host": ignore the nick
			_, target, _ = strings.Cut(prefix, "!")
		}
		owner = maskMatch(c.owner, target)
	} else {
		account := tags["account"]
		owner = c.accountTags && account != "" && account != "*" && strings.EqualFold(account, c.owner)
	}
	return owner, !owner && channels.MatchOwner(c.ownerID(), nickOf(prefix))
}

func (c *Channel) onLine(ctx context.Context, line string, handler channels.Handler) error {
	prefix, cmd, params := parse(line)
	switch cmd {
	case "PING":
		if len(params) > 0 {
			return c.raw("PONG :%s", params[len(params)-1])
		}
		return c.raw("PONG")
	case "CAP":
		return c.onCap(params)
	case "AUTHENTICATE":
		if len(params) > 0 && params[0] == "+" {
			blob := base64.StdEncoding.EncodeToString([]byte(c.nick + "\x00" + c.nick + "\x00" + c.password))
			return c.raw("AUTHENTICATE %s", blob)
		}
	case "903", "904", "905", "906", "907": // SASL done (success or failure); continue either way
		return c.raw("CAP END")
	case "001":
		c.log.Info("irc connected", "server", c.server, "nick", c.nick, "accounts", c.accountTags)
		c.mu.Lock()
		c.noAccounts = !c.accountTags
		c.mu.Unlock()
		if w := c.Warning(); w != "" {
			c.log.Warn("irc: " + w)
		}
		for _, r := range c.rooms {
			if r = strings.TrimSpace(r); r != "" {
				_ = c.raw("JOIN %s", r)
			}
		}
	case "464": // password incorrect: reconnecting won't change that
		return channels.Fatal(errors.New("the IRC server rejected the password; check it and reconnect"))
	case "433": // nick in use
		c.nick += "_"
		return c.raw("NICK %s", c.nick)
	case "NICK", "QUIT":
		c.mu.Lock()
		if c.ownerNick != "" && strings.EqualFold(nickOf(prefix), c.ownerNick) {
			c.ownerNick = "" // until the owner speaks again, verified
		}
		c.mu.Unlock()
	case "PRIVMSG":
		if len(params) < 2 {
			return nil
		}
		target, text := params[0], strings.TrimSpace(params[1])
		if strings.HasPrefix(text, "\x01") { // CTCP
			return nil
		}
		nick := nickOf(prefix)
		chatID := nick
		if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "&") {
			lower := strings.ToLower(text)
			if !strings.HasPrefix(lower, strings.ToLower(c.nick)+":") && !strings.HasPrefix(lower, strings.ToLower(c.nick)+",") {
				return nil
			}
			text = strings.TrimSpace(text[len(c.nick)+1:])
			chatID = target
		}
		if text == "" {
			return nil
		}
		isOwner, impostor := c.sender(prefix, parseTags(line))
		c.mu.Lock()
		switch {
		case isOwner:
			c.ownerNick = nick
		case strings.EqualFold(c.ownerNick, nick):
			c.ownerNick = "" // someone else holds that nick now
		}
		c.mu.Unlock()
		if impostor {
			// They would land in the owner's conversation, with its history
			// and approvals, so they aren't answered even as a stranger.
			c.log.Warn("irc: ignored a message from your nick, because it isn't logged in to your services account", "nick", nick, "prefix", prefix)
			return nil
		}
		if !isOwner && !c.replyToOthers {
			return nil
		}
		who := nick
		if isOwner {
			who = c.ownerID() // the owner's conversation, whichever nick they use
		}
		if chatID == nick {
			chatID = who
		} else {
			chatID = target + "|" + who // Send prefixes the reply with the nick
		}
		handler(ctx, channels.Inbound{Channel: "irc", ChatID: chatID, Sender: nick, Text: text, IsOwner: isOwner})
	}
	return nil
}

// onCap negotiates capabilities: account-tag always (to recognise the
// owner) and sasl when a password is set, each only if the server offers it.
func (c *Channel) onCap(params []string) error {
	if len(params) < 3 {
		return nil
	}
	list := strings.Fields(params[len(params)-1])
	switch strings.ToUpper(params[1]) {
	case "LS":
		c.offered = append(c.offered, list...)
		if len(params) >= 4 && params[2] == "*" {
			return nil // more to come
		}
		var want []string
		for _, cp := range c.offered {
			name, _, _ := strings.Cut(cp, "=")
			if name == "account-tag" || (name == "sasl" && c.password != "") {
				want = append(want, name)
			}
		}
		c.offered = nil
		if len(want) == 0 {
			return c.raw("CAP END")
		}
		return c.raw("CAP REQ :%s", strings.Join(want, " "))
	case "ACK":
		sasl := false
		for _, cp := range list {
			switch cp {
			case "account-tag":
				c.accountTags = true
			case "sasl":
				sasl = true
			}
		}
		if sasl {
			return c.raw("AUTHENTICATE PLAIN")
		}
		return c.raw("CAP END")
	case "NAK":
		return c.raw("CAP END")
	}
	return nil
}

// inbox holds messages for the worker. The owner's have a lane of their
// own and are taken first, so a busy channel or a stranger's flood never
// pushes them out.
type inbox struct{ owner, others chan channels.Inbound }

func newInbox(n int) *inbox {
	return &inbox{owner: make(chan channels.Inbound, n), others: make(chan channels.Inbound, n)}
}

// put queues in, or reports false when its lane is full.
func (b *inbox) put(in channels.Inbound) bool {
	lane := b.others
	if in.IsOwner {
		lane = b.owner
	}
	select {
	case lane <- in:
		return true
	default:
		return false
	}
}

// next waits for a message, the owner's first. It reports false once done closes.
func (b *inbox) next(done <-chan struct{}) (channels.Inbound, bool) {
	select {
	case in := <-b.owner:
		return in, true
	default:
	}
	select {
	case in := <-b.owner:
		return in, true
	case in := <-b.others:
		return in, true
	case <-done:
		return channels.Inbound{}, false
	}
}

// maskMatch matches an IRC mask with * and ? wildcards, ignoring case.
func maskMatch(mask, s string) bool {
	mask, s = strings.ToLower(mask), strings.ToLower(s)
	mi, si, star, from := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case mi < len(mask) && (mask[mi] == '?' || mask[mi] == s[si]):
			mi++
			si++
		case mi < len(mask) && mask[mi] == '*':
			star, from = mi, si
			mi++
		case star >= 0:
			mi, from = star+1, from+1
			si = from
		default:
			return false
		}
	}
	for mi < len(mask) && mask[mi] == '*' {
		mi++
	}
	return mi == len(mask)
}

// cut shortens s to at most n bytes without splitting a UTF-8 character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for i := n; i > 0 && i > n-utf8.UTFMax; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return s[:n] // not UTF-8 anyway
}

// Send delivers text as one PRIVMSG per line, paced to avoid flood limits.
// chatID is a nick, or "#room|nick" to address someone in a channel. The
// owner's conversation goes to the nick they were last verified under, and
// nowhere while there is none: someone else may hold their usual nick.
func (c *Channel) Send(_ context.Context, chatID, text string) error {
	target, addressee := chatID, ""
	if i := strings.Index(chatID, "|"); i > 0 && strings.ContainsAny(chatID[:1], "#&") {
		target, addressee = chatID[:i], chatID[i+1:]
	}
	c.mu.Lock()
	ownerNick, ownerID := c.ownerNick, c.ownerID()
	c.mu.Unlock()
	switch {
	case addressee == ownerID && ownerNick != "":
		addressee = ownerNick
	case addressee == "" && target == ownerID:
		if ownerNick == "" {
			return errors.New("irc: you aren't on IRC right now (or haven't spoken since connecting), so the message wasn't sent")
		}
		target = ownerNick
	}
	var lines []string
	for _, l := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		for len(l) > 400 {
			head := cut(l, 400)
			if sp := strings.LastIndex(head, " "); sp >= 200 {
				head = head[:sp]
			}
			lines = append(lines, head)
			l = strings.TrimSpace(l[len(head):])
		}
		lines = append(lines, l)
	}
	for i, l := range lines {
		if addressee != "" && i == 0 {
			l = addressee + ": " + l
		}
		c.mu.Lock()
		wait := time.Until(c.last.Add(600 * time.Millisecond))
		c.mu.Unlock()
		if wait > 0 {
			time.Sleep(wait)
		}
		if err := c.raw("PRIVMSG %s :%s", target, l); err != nil {
			return err
		}
		c.mu.Lock()
		c.last = time.Now()
		c.mu.Unlock()
	}
	return nil
}
