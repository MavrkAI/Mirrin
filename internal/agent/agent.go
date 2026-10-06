// Package agent is Mirrin's brain: it turns an incoming message into model
// calls, tool calls and a reply, gated by the approvals policy.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Agent orchestrates a conversation.
type Agent struct {
	cfg      *config.Config
	provider llm.Provider
	store    *memory.Store
	tools    *tools.Registry
	policy   *approvals.Policy
	log      *slog.Logger

	// StrangerAsked, if set, hears about each approval raised for someone
	// other than the owner, so the owner can be told wherever they are.
	StrangerAsked func(ctx context.Context, ap memory.Approval, who string)
	// MaxIterations bounds tool-call loops per turn.
	MaxIterations int
	// RuntimeInfo, if set, describes how Mirrin is currently running (mode, channels).
	RuntimeInfo func() string
	// Location, if set, is the time zone the owner is in now: it follows the
	// system as a laptop travels, where the config's zone is read once. Set
	// it before the first turn; turns read it from the live agent.
	Location func() *time.Location
	// Budgets caps tool calls per request by size of the ask (zero value = DefaultBudgets).
	Budgets Budgets
	// Browser, if set, says what the twin's browser has open, in a line for
	// the prompt, or "" when nothing is. One browser serves every chat, so a
	// voice turn knows what the screen's turn left open, and a turn after
	// the browser closed doesn't carry on as if it hadn't.
	Browser func(ctx context.Context) string
	// Tasks, if set, says where the background tasks stand right now, for
	// the prompt: the conversation may say a booking is under way after the
	// owner dropped it.
	Tasks func() string
	// CheckApproved, if set, runs just before an approved call executes; an
	// error means the world has moved on and the call is not run.
	CheckApproved func(ctx context.Context, ap memory.Approval) error
	// Performed, if set, hears that an approved call has run, and whether
	// it worked, so the screens react only to what really happened.
	Performed func(ctx context.Context, ap memory.Approval, ok bool)

	voiceProvider llm.Provider
	persona       personaData

	evmu   sync.Mutex
	events map[string]Events
	hooks  approvalHooks // what hears about approvals (OnApproval, hooks.go)

	// mu guards what the tray can change while turns run (cfg, provider,
	// voiceProvider, policy, persona); each turn works on its own copy.
	mu    sync.RWMutex
	root  *Agent     // set on a turn's copy: the live agent it came from
	query string     // on a turn's copy: the message being answered
	facts factsCache // on a turn's copy: what it remembers, until facts change
}

// New wires an Agent. It keeps its own copy of cfg; use SetConfig to change it.
func New(cfg *config.Config, provider llm.Provider, store *memory.Store, reg *tools.Registry, policy *approvals.Policy, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	own := *cfg
	return &Agent{cfg: &own, provider: provider, store: store, tools: reg, policy: policy, log: log, MaxIterations: 25, events: map[string]Events{}}
}

// SetPersona sets who the twin is: name, how it's spoken, character, address, style.
func (a *Agent) SetPersona(name, spoken, character, address string, style []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.persona = personaData{Name: name, Spoken: spoken, Character: character, Address: address, Style: style}
}

// SetProvider swaps the model backend (e.g. after a config change).
func (a *Agent) SetProvider(p llm.Provider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.provider = p
}

// SetVoiceProvider sets a faster backend used for spoken conversations (nil to share the main one).
func (a *Agent) SetVoiceProvider(p llm.Provider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.voiceProvider = p
}

func (a *Agent) providerFor(chatKey string) llm.Provider {
	if a.voiceProvider != nil && strings.HasPrefix(chatKey, "voice:") {
		return a.voiceProvider
	}
	return a.provider
}

// Provider returns the active model backend.
func (a *Agent) Provider() llm.Provider {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.provider
}

// SetPolicy swaps the approvals policy.
func (a *Agent) SetPolicy(p *approvals.Policy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.policy = p
}

// PortraitPrompt is the task that writes or refreshes the portrait.
const PortraitPrompt = `Write a short portrait of the user in your own voice: who they are, what their days look like, what they care about, what they find hard, how they like to be spoken to. Draw only on what you actually know (memory, recent conversations). If something isn't in memory any more, leave it out, even if an old conversation mentions it. Four to eight sentences, second person is fine ("You..."), no lists, no headings, nothing sensitive they haven't shared with you directly. Reply with the portrait text, then one last line on its own: "NEW: <one sentence on what's new since your last portrait, or NONE>".`

// WritePortrait asks the model for a fresh portrait and stores it. news is
// its line on what's new since the last one, kept apart from the portrait
// (empty for NONE).
func (a *Agent) WritePortrait(ctx context.Context, chatKey string) (text, news string, err error) {
	reply, err := a.run(ctx, chatKey, llm.Text(llm.RoleUser, "[Scheduled task: refresh your portrait of the user. Do not address the user; output only the portrait and its NEW line.]\n\n"+PortraitPrompt), nil)
	if err != nil {
		return "", "", err
	}
	text, news = splitPortrait(reply) // portrait.go
	if strings.Contains(text, "NOTHING_TO_REPORT") || len(text) < 40 {
		return "", "", fmt.Errorf("portrait too short")
	}
	if err := a.store.SetPortrait(ctx, text); err != nil {
		return "", "", err
	}
	_ = a.store.Unset(ctx, memory.KeyPortraitAside) // a fresh portrait is no longer set aside (daemon/portrait.go)
	a.store.Audit(ctx, "memory.portrait", chatKey, truncate(text, 200))
	return text, news, nil
}

// Tools exposes the registry so skills can be added after construction.
func (a *Agent) Tools() *tools.Registry { return a.tools }

// Store exposes memory.
func (a *Agent) Store() *memory.Store { return a.store }

// Config exposes configuration (read-only: change it with SetConfig).
func (a *Agent) Config() *config.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// Handle processes a user message and returns the reply text.
func (a *Agent) Handle(ctx context.Context, chatKey, userText string) (string, error) {
	return a.run(ctx, chatKey, llm.Text(llm.RoleUser, userText), nil)
}

// HandleStreaming is Handle with text delivered as it is generated. onDelta
// receives fragments of every assistant turn (including remarks made before
// a tool call), so a voice or terminal channel can start immediately.
func (a *Agent) HandleStreaming(ctx context.Context, chatKey, userText string, onDelta func(string)) (string, error) {
	return a.run(ctx, chatKey, llm.Text(llm.RoleUser, userText), onDelta)
}

// Events lets a channel follow a turn as it happens.
type Events struct {
	// OnDelta receives reply text as it is generated.
	OnDelta func(text string)
	// OnTool is called just before a tool runs, with a short spoken-style caption.
	OnTool func(tool, caption string)
}

// HandleEvents is Handle with streaming text and tool narration.
func (a *Agent) HandleEvents(ctx context.Context, chatKey, userText string, ev Events) (string, error) {
	a.evmu.Lock()
	a.events[chatKey] = ev
	a.evmu.Unlock()
	defer func() {
		a.evmu.Lock()
		delete(a.events, chatKey)
		a.evmu.Unlock()
	}()
	return a.run(ctx, chatKey, llm.Text(llm.RoleUser, userText), ev.OnDelta)
}

func (a *Agent) toolEvent(chatKey string) func(string, string) {
	if a.root != nil {
		return a.root.toolEvent(chatKey)
	}
	a.evmu.Lock()
	defer a.evmu.Unlock()
	if ev, ok := a.events[chatKey]; ok {
		return ev.OnTool
	}
	return nil
}

// RunTask runs a system-initiated prompt (a protocol, a heartbeat check) in the
// context of a chat. The prompt is framed so the model knows nobody typed it.
func (a *Agent) RunTask(ctx context.Context, chatKey, task string) (string, error) {
	text := "[Scheduled task from your heartbeat, not typed by the user. Do the task and reply with what the user should see. If nothing needs saying, reply exactly: NOTHING_TO_REPORT]\n\n" + task
	return a.run(ctx, chatKey, llm.Text(llm.RoleUser, text), nil)
}

func (a *Agent) run(ctx context.Context, chatKey string, incoming llm.Message, onDelta func(string)) (string, error) {
	incoming = withPhotos(ctx, incoming) // a photo sent with the message (photos.go)
	a = a.turn(incoming.PlainText())     // settings changed mid-turn apply from the next turn
	budget := a.toolBudget(ctx, chatKey, incoming.PlainText())
	toolCalls := 0
	if err := a.store.AppendMessage(ctx, chatKey, incoming); err != nil {
		return "", err
	}
	system := a.systemFor(ctx)
	var finalText strings.Builder
	// Screenshots are not persisted with history; keep this run's by tool-use id
	// and re-attach them after each reload so the model can still see them.
	images := map[string]llm.Block{}

	maxIter := a.MaxIterations
	if inTask(chatKey) && maxIter < 80 {
		maxIter = 80
	}
	turns := a.cfg.LLM.HistoryTurns
	if inTask(chatKey) && turns < 400 {
		turns = 400 // a task is one long tool loop; its brief must stay in view
	}
	var stop llm.StopReason
	outOfSteps := false
	for i := 0; i < maxIter; i++ {
		history, err := a.store.History(ctx, chatKey, turns)
		if err != nil {
			return "", err
		}
		history = repair(history)
		history = a.showPhotos(ctx, a.providerFor(chatKey), history) // photos.go
		if len(images) > 0 {
			for mi := range history {
				for bi, b := range history[mi].Blocks {
					if src, ok := images[b.ToolUseID]; ok && b.Type == llm.BlockToolResult {
						history[mi].Blocks[bi].Image, history[mi].Blocks[bi].ImageType = src.Image, src.ImageType
					}
				}
			}
		}
		req := llm.Request{
			System:         system,
			SystemVolatile: a.volatile(ctx) + channelStyle(chatKey),
			Messages:       history,
			Tools:          a.tools.Specs(),
			MaxTokens:      a.cfg.LLM.MaxTokens,
			Effort:         a.effortFor(chatKey),
		}
		var resp *llm.Response
		provider := a.providerFor(chatKey)
		if onDelta != nil {
			resp, err = llm.CompleteStreaming(ctx, provider, req, onDelta)
		} else {
			resp, err = provider.Complete(ctx, req)
		}
		if err != nil {
			a.store.Audit(ctx, "llm.error", chatKey, err.Error())
			return "", a.photosRefused(req.Messages, err) // photos.go
		}
		a.recordUsage(ctx, chatKey, provider, resp)
		if err := a.store.AppendMessage(ctx, chatKey, resp.Message); err != nil {
			return "", err
		}
		if t := resp.Message.PlainText(); t != "" {
			if finalText.Len() > 0 {
				finalText.WriteString("\n")
			}
			finalText.WriteString(t)
		}
		if stop = resp.StopReason; stop != llm.StopToolUse {
			break
		}
		if onDelta != nil && resp.Message.PlainText() != "" {
			onDelta("\n") // end of a spoken holding line: let the voice flush it now
		}

		results := llm.Message{Role: llm.RoleUser}
		// Keep the final call for saying where things stand; tools run then would go unseen.
		lastCall := i == maxIter-1 || maxIter > 2 && i >= maxIter-2
		for _, b := range resp.Message.Blocks {
			if b.Type != llm.BlockToolUse {
				continue
			}
			toolCalls++
			if lastCall {
				outOfSteps = true
				results.Blocks = append(results.Blocks, stepLimitResult(b.ToolUseID))
				continue
			}
			if budget > 0 && toolCalls > budget {
				a.store.Audit(ctx, "tool.budget", chatKey, fmt.Sprintf("%s refused: %d-call budget used", b.ToolName, budget))
				results.Blocks = append(results.Blocks, llm.Block{Type: llm.BlockToolResult, ToolUseID: b.ToolUseID, IsError: true,
					Text: fmt.Sprintf("Tool budget for this request is used up (%d call(s) for a request of this size). Answer with what you already have, in one or two sentences, and offer to dig further if the user wants.", budget)})
				continue
			}
			if onTool := a.toolEvent(chatKey); onTool != nil {
				onTool(b.ToolName, caption(b.ToolName, b.Input))
			}
			text, isErr := a.execute(ctx, chatKey, b)
			blk := llm.Block{Type: llm.BlockToolResult, ToolUseID: b.ToolUseID, IsError: isErr}
			blk.Text, blk.Image, blk.ImageType = liftImage(text, a.cfg.DataDir)
			if isErr {
				// The API rejects images on error results; the text still names the file.
				blk.Image, blk.ImageType = nil, ""
			}
			if blk.Image != nil {
				images[blk.ToolUseID] = blk
			}
			results.Blocks = append(results.Blocks, blk)
		}
		if len(results.Blocks) == 0 {
			break
		}
		if err := a.store.AppendMessage(ctx, chatKey, results); err != nil {
			return "", err
		}
		// Text emitted before a tool call is commentary; the real answer comes after.
		finalText.Reset()
	}
	return a.reply(chatKey, strings.TrimSpace(finalText.String()), stop, outOfSteps)
}

// execute runs one tool call through the approvals gate.
func (a *Agent) execute(ctx context.Context, chatKey string, b llm.Block) (string, bool) {
	tool, ok := a.tools.Get(b.ToolName)
	if !ok {
		return fmt.Sprintf("unknown tool %q", b.ToolName), true
	}
	call := tools.Call{ChatKey: chatKey, Input: b.Input}
	risk := tool.Risk()
	if cr, ok := tool.(tools.CallRisker); ok {
		risk = cr.RiskFor(ctx, call)
	}
	risk = a.safetyFloor(b.ToolName, risk)
	// A call that could never work, or must never run, isn't put to the
	// owner, and doesn't run on an "always allow" either.
	if c, ok := tool.(tools.Checker); ok {
		if err := c.Check(ctx, call); err != nil {
			a.store.Audit(ctx, "tool.refused", chatKey, fmt.Sprintf("%s: %s", b.ToolName, err))
			return err.Error(), true
		}
	}
	switch a.decide(ctx, chatKey, tool, risk) {
	case approvals.Deny:
		a.store.Audit(ctx, "tool.denied_by_policy", chatKey, b.ToolName)
		return fmt.Sprintf("%s is disabled by the user's autonomy policy. Explain that you are not permitted to do this.", b.ToolName), true
	case approvals.Ask:
		return a.ask(ctx, chatKey, tool, b.ToolName, b.Input, risk)
	}
	start := time.Now()
	result, err := tool.Run(ctx, call)
	if err != nil {
		a.store.Audit(ctx, "tool.failed", chatKey, fmt.Sprintf("%s: %s", b.ToolName, err))
		return err.Error(), true
	}
	a.store.Audit(ctx, "tool.ok", chatKey, fmt.Sprintf("%s (%s) %s", b.ToolName, time.Since(start).Round(time.Millisecond), auditInput(b.ToolName, b.Input, result, 300)))
	if result == "" {
		result = "(no output)"
	}
	return truncate(result, 60000), false
}

// volatile is the part of the system prompt that changes per request.
func (a *Agent) volatile(ctx context.Context) string {
	loc := a.location()
	var b strings.Builder
	fmt.Fprintf(&b, "Current time: %s (%s)\n", time.Now().In(loc).Format("Monday 2 January 2006, 15:04"), loc.String())
	if who, ok := stranger(ctx); ok {
		fmt.Fprintf(&b, "You are talking with %q, not %s: nothing you remember about %s is shown here, and every tool needs %s's approval.\n", who, a.principal(), a.principal(), a.principal())
		return b.String()
	}
	if a.RuntimeInfo != nil {
		if info := a.RuntimeInfo(); info != "" {
			fmt.Fprintf(&b, "Runtime: %s\n", info)
		}
	}
	if a.Tasks != nil {
		if ts := a.Tasks(); ts != "" {
			fmt.Fprintf(&b, "Background tasks right now (this overrides anything older in the conversation): %s Read each task's dates against today: a task for a day that has passed (a booking 'tomorrow' started days ago, a flight already gone) is moot, so say so plainly and offer to drop it, and never ask for details to finish it.\n", ts)
		}
	}
	if a.Browser != nil {
		if br := a.Browser(ctx); br != "" {
			fmt.Fprintf(&b, "Your browser (one browser, shared by every chat): %s You can always read the page it's on yourself with browse_page and no url; never ask the user to read a page out to you or to pick for you from something you haven't looked at.\n", br)
		} else {
			b.WriteString("Your browser (one browser, shared by every chat): closed, with no page open. Any page earlier in the conversation is gone; to show the user a page, open it with a browser tool first, and never say a page is open until a tool has opened it.\n")
		}
	}
	if p, err := a.store.GetPortrait(ctx); err == nil && p.Text != "" {
		fmt.Fprintf(&b, "\nPortrait of the user (your own words, updated %s):\n%s\n", p.UpdatedAt.Format("2 Jan"), p.Text)
	}
	b.WriteString(a.memoryBlock(ctx))
	return b.String()
}

// Budgets are the maximum tool calls per request, by how big the ask is. Zero means unlimited.
type Budgets struct {
	CheckIn  int // "what's happening", "how's it going"
	Question int // anything asked as a question
	Request  int // an instruction
	Protocol int // a named protocol or scratch run
	Task     int // a background task leg
}

// browsingBudget is the least a question or request gets while the browser
// has a page open: stepping through a site (search, pick, fill in) takes
// more calls than an ordinary ask, and stopping halfway strands the owner.
const browsingBudget = 15

// DefaultBudgets is a butler's discretion in numbers.
var DefaultBudgets = Budgets{CheckIn: 1, Question: 3, Request: 6, Protocol: 25, Task: 60}

var reCheckIn = regexp.MustCompile(`(?i)^\s*(what'?s (happening|up|new|going on)|how'?s it going|how are (you|things)|anything (new|happening|up)|status|any news|all good)\W*$`)
var reQuestion = regexp.MustCompile(`(?i)^\s*(what|who|when|where|why|how|which|is|are|can|could|do|does|did|will|would|should|have|has)\b`)

// classify decides how big a request is.
func classify(text string) string {
	t := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(t, "["): // system-framed task (scheduled, approval outcome)
		return "protocol"
	case reCheckIn.MatchString(t):
		return "checkin"
	case len(strings.Fields(t)) <= 4 && strings.HasSuffix(t, "?"):
		return "checkin"
	case strings.HasSuffix(t, "?") || reQuestion.MatchString(t):
		return "question"
	}
	return "request"
}

func (a *Agent) toolBudget(ctx context.Context, chatKey, text string) int {
	if _, ok := stranger(ctx); ok {
		return strangerBudget // strangers.go: whatever the text claims to be
	}
	b := a.Budgets
	if b == (Budgets{}) {
		b = DefaultBudgets
	}
	if inTask(chatKey) {
		if b.Task > 0 {
			return b.Task
		}
		return 60
	}
	if memory.IsScratch(chatKey) { // scratch runs: protocols, watchers, jobs (not an IRC room)
		return b.Protocol
	}
	n := b.Request
	browsing := func() bool { return a.Browser != nil && a.Browser(ctx) != "" }
	switch classify(text) {
	case "checkin":
		// A short question about the page ("which flights are there?")
		// needs a look at it; only a real "how's it going" stays a glance.
		if reCheckIn.MatchString(text) || !browsing() {
			return b.CheckIn
		}
		n = b.Question
	case "question":
		n = b.Question
	case "protocol":
		return b.Protocol
	}
	if n > 0 && n < browsingBudget && browsing() {
		n = browsingBudget
	}
	return n
}

// caption is the three-word narration for a tool call.
func caption(tool string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string {
		if v, ok := in[k]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}
	switch tool {
	case "list_events":
		return "Checking your calendar."
	case "create_event", "update_event", "delete_event":
		return "Updating your calendar."
	case "list_emails", "read_email", "list_sent_emails", "read_sent_email":
		return "Reading your email."
	case "send_email", "reply_email":
		return "Drafting the email."
	case "fetch_url":
		if u := str("url"); u != "" {
			return "Looking at " + hostOf(u) + "."
		}
		return "Looking that up."
	case "browse_page", "browser_act", "browser_inspect", "screenshot_page":
		return "Opening the site."
	case "browser_signin":
		return "Handing you the browser to sign in."
	case "start_task":
		if n := str("title"); n != "" {
			return "Starting on " + n + " in the background."
		}
		return "Starting a background task."
	case "task_update":
		return ""
	case "teach_start":
		return "Watching you do it."
	case "teach_stop":
		return "Writing down the steps."
	case "gmail_search", "gmail_read":
		return "Reading your email."
	case "gmail_send", "gmail_reply":
		return "Drafting the email."
	case "drive_search", "drive_read":
		return "Looking in your Drive."
	case "phone_call":
		return "Making the call."
	case "send_sms":
		return "Sending the text."
	case "check_spend", "record_spend":
		return "Checking the budget."
	case "create_tool":
		if n := str("name"); n != "" {
			return "Writing myself a tool: " + n + "."
		}
		return "Writing myself a tool."
	case "run_shell":
		return "Running that command."
	case "read_file", "list_dir", "write_file":
		return "Checking the files."
	case "run_protocol":
		if n := str("name"); n != "" {
			return "Running " + n + "."
		}
		return "Running the protocol."
	case "find_protocols":
		return "Looking in the registry."
	case "install_pack":
		return "Installing the pack."
	case "set_reminder", "follow_up", "list_reminders", "cancel_reminder", "remember", "recall", "forget", "list_protocols", "remember_sensitive", "create_protocol", "update_protocol":
		return "" // instant; no narration
	}
	if i := strings.Index(tool, "__"); i > 0 {
		return "Using " + tool[:i] + "."
	}
	return "One moment."
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	u = strings.TrimPrefix(u, "www.")
	if i := strings.IndexAny(u, "/?"); i > 0 {
		u = u[:i]
	}
	return u
}

// effortFor picks the thinking effort by medium: voice favours speed.
func (a *Agent) effortFor(chatKey string) string {
	if strings.HasPrefix(chatKey, "voice:") && a.cfg.LLM.VoiceEffort != "" {
		return a.cfg.LLM.VoiceEffort
	}
	return a.cfg.LLM.Effort
}

// channelStyle adds delivery guidance for the medium the user is on.
func channelStyle(chatKey string) string {
	switch {
	case strings.HasPrefix(chatKey, "voice:"):
		return `
Delivery: you are SPEAKING out loud and the user is listening, not reading. Talk like a person in the room: default to ONE short sentence, two at most unless they ask for detail; contractions, no lists, no markdown, no URLs, no ids or numbers the user would have to write down. Put the answer in the first six words. Say times like "half past two". For anything needing approval, describe it in a phrase and ask "shall I?" (the user just says yes or no). If a longer answer is genuinely needed, give the headline and offer to go on.
Work quietly: the system already acknowledges the user, and the steps show on the screen. Don't narrate what you're doing ("opening the site", "still loading", "now clicking the dates", "one moment"): write nothing before or between tool calls, and speak once, when you have the answer or need the user. Only if something will take minutes, say so once, in a few words. Casual check-ins ("what's happening", "how's it going", "anything new") mean your status: reminders, upcoming events, anything you noticed; answer from what you already know and don't go to the web unless asked.
`
	case strings.HasPrefix(chatKey, "mail:"):
		return "\nDelivery: an email reply. Plain text, a short paragraph or two, no markdown headings; no greeting or sign-off needed.\n"
	case strings.HasPrefix(chatKey, "irc:"):
		return "\nDelivery: IRC. One to three short lines, plain text, no markdown.\n"
	case strings.HasPrefix(chatKey, "slack:"), strings.HasPrefix(chatKey, "discord:"), strings.HasPrefix(chatKey, "mattermost:"), strings.HasPrefix(chatKey, "zulip:"), strings.HasPrefix(chatKey, "matrix:"):
		return "\nDelivery: a chat app. Short, plain text, at most a few lines; simple markdown (bold, bullets) is fine, no headings or tables.\n"
	case channels.IsMessaging(chatKey):
		return "\nDelivery: a messaging app on a phone. Short, plain text, at most a few lines.\n"
	}
	return ""
}

// repair drops leading orphaned tool results and trailing unanswered tool
// uses so a truncated history is still a valid conversation.
func repair(h []llm.Message) []llm.Message {
	orig := h
	for len(h) > 0 {
		first := h[0]
		if first.Role == llm.RoleUser && len(first.Blocks) > 0 && first.Blocks[0].Type == llm.BlockToolResult {
			h = h[1:]
			continue
		}
		if first.Role == llm.RoleAssistant {
			h = h[1:]
			continue
		}
		break
	}
	if len(h) == 0 && len(orig) > 0 {
		// The window opened mid tool-loop and no plain user message survived
		// (a long task). Keep the loop from the first assistant message and
		// stand in for the trimmed start so the conversation is still valid.
		start := 0
		for start < len(orig) && orig[start].Role != llm.RoleAssistant {
			start++
		}
		h = append([]llm.Message{llm.Text(llm.RoleUser, "[Earlier messages trimmed for length. Continue from your board and the recent tool results.]")}, orig[start:]...)
	}
	// Answer any assistant tool_use whose result is missing (e.g. after a crash),
	// and drop tool_results that don't answer the immediately preceding tool_use
	// (left behind when a run was interrupted or interleaved).
	out := make([]llm.Message, 0, len(h))
	var expecting map[string]bool // tool_use ids from the last assistant message
	for i, m := range h {
		if m.Role == llm.RoleUser {
			kept := m.Blocks[:0:0]
			for _, b := range m.Blocks {
				if b.Type == llm.BlockToolResult && !expecting[b.ToolUseID] {
					continue
				}
				kept = append(kept, b)
			}
			m.Blocks = kept
			if len(m.Blocks) == 0 {
				continue
			}
			out = append(out, m)
			expecting = nil
			continue
		}
		out = append(out, m)
		if m.Role != llm.RoleAssistant {
			continue
		}
		expecting = map[string]bool{}
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse {
				expecting[b.ToolUseID] = true
			}
		}
		var pending []string
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse {
				pending = append(pending, b.ToolUseID)
			}
		}
		if len(pending) == 0 {
			continue
		}
		answered := i+1 < len(h) && h[i+1].Role == llm.RoleUser && len(h[i+1].Blocks) > 0 && h[i+1].Blocks[0].Type == llm.BlockToolResult
		if !answered {
			fill := llm.Message{Role: llm.RoleUser}
			for _, id := range pending {
				fill.Blocks = append(fill.Blocks, llm.Block{Type: llm.BlockToolResult, ToolUseID: id, Text: "(result lost: the service restarted before this tool finished)", IsError: true})
			}
			out = append(out, fill)
		}
	}
	// Merge consecutive same-role messages, which the API rejects.
	merged := make([]llm.Message, 0, len(out))
	for _, m := range out {
		if n := len(merged); n > 0 && merged[n-1].Role == m.Role {
			merged[n-1].Blocks = append(merged[n-1].Blocks, m.Blocks...)
			continue
		}
		merged = append(merged, m)
	}
	return merged
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) { // never cut a character in half
		n--
	}
	return s[:n] + "…[truncated]"
}

// NotAnAnswer reports whether the owner's message is plainly not the answer
// to a question a task put to them: a check-in ("how are you doing?") or a
// question of their own. Those go to a normal turn, where the model can
// still pass on an answer with answer_task.
func NotAnAnswer(text string) bool {
	t := strings.TrimSpace(text)
	return reCheckIn.MatchString(t) || reSmallTalk.MatchString(t) || strings.HasSuffix(t, "?")
}

var reSmallTalk = regexp.MustCompile(`(?i)^\s*(hi|hey|hello|hiya|yo|thanks|thank you|cheers|good (morning|afternoon|evening|night)|how('?re| are) you( doing)?|are you (there|online|awake))\W*$`)
