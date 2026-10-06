// Package tasks runs long jobs in the background: "plan the Bali trip",
// "chase the refund", "find me a plumber". Each task is its own scratch
// conversation with a big tool budget, a board of steps the model keeps
// current, and pauses whenever it needs the user: an approval, an answer, a
// login. Tasks are persisted and resume after a restart.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/events"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Status of a task.
type Status string

const (
	Running         Status = "running"
	WaitingApproval Status = "waiting_approval"
	WaitingUser     Status = "waiting_user"
	Done            Status = "done"
	Failed          Status = "failed"
	Cancelled       Status = "cancelled"
	// Paused means the task was set aside (PausedBy says why); retry carries on.
	Paused Status = "paused"
)

// Why a paused task was set aside (Task.PausedBy).
const (
	// PausedBudget: the monthly model budget ran out. The owner was told it
	// carries on when the budget allows, so it is picked up by itself.
	PausedBudget = "budget"
	// PausedSteps: a leg used every step it was allowed.
	PausedSteps = "steps"
	// PausedLapsed: a request for approval went unanswered too long.
	PausedLapsed = "lapsed"
)

// budgetPausedFor is PausedFor for a task paused for the budget.
const budgetPausedFor = "the monthly model budget was used up; it carries on when the budget allows"

// Step is one line on the board.
type Step struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// Task is one background job.
type Task struct {
	ID       string    `json:"id"`
	Owner    string    `json:"owner"` // the chat key that asked, where updates go
	Key      string    `json:"key"`   // the scratch conversation the work happens in
	Title    string    `json:"title"`
	Goal     string    `json:"goal"`
	Status   Status    `json:"status"`
	Steps    []Step    `json:"steps"`
	Notes    []string  `json:"notes"`
	Question string    `json:"question,omitempty"`
	Result   string    `json:"result,omitempty"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	AskedAt  time.Time `json:"asked_at,omitempty"`
	Runs     int       `json:"runs"`
	// PausedFor says why a paused task stopped, when it wasn't running out
	// of steps (an approval that lapsed unanswered); retry tells the model.
	PausedFor string `json:"paused_for,omitempty"`
	// PausedBy is what set a paused task aside: PausedBudget, PausedSteps or
	// PausedLapsed. Only a budget pause carries on by itself; the others
	// wait for the owner to try again.
	PausedBy string `json:"paused_by,omitempty"`
}

// ErrOverBudget pauses a task because the monthly model budget is used up.
// It isn't a failure or a lack of steps, and the owner is told once.
var ErrOverBudget = errors.New("paused for the monthly model budget")

// Runner drives one leg of a task: input is what the model should see next.
type Runner func(ctx context.Context, t *Task, input string) (string, error)

// Deps are the daemon services the manager uses.
type Deps struct {
	Run      Runner
	Notify   func(ctx context.Context, chatKey, text string) error
	Pending  func(ctx context.Context, chatKey string) (int, string) // pending approvals in a chat: count and a one-line summary
	Load     func(ctx context.Context) (string, error)
	Save     func(ctx context.Context, data string) error
	Parallel int
	Log      *slog.Logger

	// OnOutcome, if set, hears once that a task finished (Done) or stopped
	// after a problem (Failed), as the owner is told. It runs after the
	// manager lets go of its lock.
	OnOutcome func(t Task, status Status)
}

// Manager owns the tasks.
type Manager struct {
	deps  Deps
	mu    sync.Mutex
	tasks map[string]*Task
	legs  map[string]*legs // by task id
	slots chan struct{}
	seq   int
}

// legs runs one task's legs one after another, never two at once on the
// same conversation, and lets Cancel stop the one in flight.
type legs struct {
	run    sync.Mutex         // held for the whole of a leg
	queued int                // legs waiting behind the current one (guarded by Manager.mu)
	cancel context.CancelFunc // stops the leg in flight (guarded by Manager.mu)
}

// New builds a manager and loads persisted tasks.
func New(ctx context.Context, deps Deps) *Manager {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Parallel <= 0 {
		deps.Parallel = 2
	}
	m := &Manager{deps: deps, tasks: map[string]*Task{}, legs: map[string]*legs{}, slots: make(chan struct{}, deps.Parallel)}
	if deps.Load != nil {
		if data, err := deps.Load(ctx); err == nil && data != "" {
			var list []*Task
			if json.Unmarshal([]byte(data), &list) == nil {
				for _, t := range list {
					if t.Status == Paused && t.PausedBy == "" { // saved before PausedBy
						t.PausedBy = pauseCause(t.PausedFor)
					}
					m.tasks[t.ID] = t
				}
			}
		}
	}
	return m
}

// pauseCause works out PausedBy for a task paused before it was recorded.
func pauseCause(pausedFor string) string {
	switch {
	case pausedFor == budgetPausedFor:
		return PausedBudget
	case pausedFor != "":
		return PausedLapsed
	}
	return PausedSteps
}

func (m *Manager) persist() {
	if m.deps.Save == nil {
		return
	}
	list := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		list = append(list, t)
	}
	data, _ := json.Marshal(list)
	if err := m.deps.Save(context.Background(), string(data)); err != nil {
		m.deps.Log.Warn("tasks: save", "err", err)
	}
}

// ResumeAll restarts tasks that were running when the process stopped.
func (m *Manager) ResumeAll(ctx context.Context) {
	m.mu.Lock()
	var again []*Task
	var briefs []string
	for _, t := range m.tasks {
		if t.Status == Running {
			again = append(again, t)
			briefs = append(briefs, t.brief())
		}
	}
	m.mu.Unlock()
	for i, t := range again {
		m.drive(ctx, t, "[Resumed after a restart. Continue from the first step on your board that is not done.]\n"+briefs[i])
	}
}

// brief restates the goal and the board, so a leg that starts after a long
// wait or a restart knows what it is doing even if early history is gone.
func (t Task) brief() string {
	return fmt.Sprintf("Goal: %s\nBoard:\n%s", t.Goal, t.Board())
}

// Start creates a task and begins working on it.
func (m *Manager) Start(ctx context.Context, owner, title, goal string) (*Task, error) {
	if strings.Contains(owner, "#task-") {
		return nil, errors.New("you are already inside a task; do the work here rather than starting another")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("give the task a short title")
	}
	m.mu.Lock()
	active := 0
	for _, t := range m.tasks {
		if t.Status == Running || t.Status == WaitingApproval || t.Status == WaitingUser {
			active++
		}
	}
	if active >= 6 {
		m.mu.Unlock()
		return nil, errors.New("six tasks are already open; finish or cancel one first")
	}
	m.seq++
	id := fmt.Sprintf("%s%d", time.Now().Format("0102"), m.seq)
	for m.tasks[id] != nil {
		m.seq++
		id = fmt.Sprintf("%s%d", time.Now().Format("0102"), m.seq)
	}
	t := &Task{ID: id, Owner: owner, Key: owner + "#task-" + id, Title: title, Goal: goal, Status: Running, Created: time.Now(), Updated: time.Now()}
	m.tasks[id] = t
	m.persist()
	m.mu.Unlock()
	m.drive(ctx, t, fmt.Sprintf("[Background task started by the user.]\nTitle: %s\nGoal: %s\n\nWork through this with your tools. First call task_update with add_steps to put your plan on the board (three to eight short steps), then do them, marking each with step_done and adding brief notes on what you found. If you need the user for anything (a decision, information, a login via browser_signin, or a payment), call task_update with ask_user and then stop; you will be resumed with their answer. When the goal is met, call task_update with finish and a result the user can read in a few lines. If it cannot be done, finish with what you found and why.", title, goal))
	return t, nil
}

// drive runs one leg in the background and then decides what the task is waiting on.
func (m *Manager) drive(ctx context.Context, t *Task, input string) {
	m.start(ctx, t, func(c context.Context) (string, error) { return m.deps.Run(c, t, input) }, legOpts{})
}

// Resume continues a task with work supplied by the caller (an approval result, say).
func (m *Manager) Resume(ctx context.Context, t *Task, step func(context.Context) (string, error)) {
	m.ResumeOr(ctx, t, step, nil)
}

// ResumeOr is Resume, calling dropped instead of step if the leg never gets
// to run because the task was cancelled or finished while it waited for its
// turn: whoever handed over the step (an approved call) can say it wasn't done.
//
// A resumed leg is mid-task work. If the model then stops without finishing
// or asking while its board still has open steps, it is sent on from the
// board once more rather than the task being closed on a half-done board.
func (m *Manager) ResumeOr(ctx context.Context, t *Task, step func(context.Context) (string, error), dropped func()) {
	m.start(ctx, t, step, legOpts{dropped: dropped, midTask: true})
}

// legOpts shape one leg.
type legOpts struct {
	dropped func() // the leg never ran
	midTask bool   // carries on work in progress (ResumeOr)
}

// start queues a leg behind any leg of the same task that is still running.
func (m *Manager) start(ctx context.Context, t *Task, step func(context.Context) (string, error), opts legOpts) {
	m.mu.Lock()
	l := m.legs[t.ID]
	if l == nil {
		l = &legs{}
		m.legs[t.ID] = l
	}
	l.queued++
	m.mu.Unlock()
	go func() {
		l.run.Lock()
		defer l.run.Unlock()
		m.slots <- struct{}{}
		defer func() { <-m.slots }()
		m.leg(ctx, t, l, step, opts)
	}()
}

// outOfSteps reports whether a leg ended because it used every step it was
// allowed (the agent's StepLimitError).
func outOfSteps(err error) bool {
	var e interface{ OutOfSteps() bool }
	return errors.As(err, &e) && e.OutOfSteps()
}

func (m *Manager) leg(ctx context.Context, t *Task, l *legs, step func(context.Context) (string, error), opts legOpts) {
	m.mu.Lock()
	l.queued--
	if t.Status == Cancelled || t.Status == Done {
		m.mu.Unlock()
		if opts.dropped != nil {
			opts.dropped()
		}
		return
	}
	t.Status = Running
	t.Runs++
	t.Updated = time.Now()
	m.persist()
	lctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	l.cancel = cancel
	m.mu.Unlock()
	out, err := step(lctx)
	cancel()
	var then func() // what to start once the lock is let go
	m.mu.Lock()
	defer func() {
		m.mu.Unlock()
		if then != nil {
			then()
		}
	}()
	l.cancel = nil
	if t.Status == Cancelled {
		return
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		// The twin is shutting down: leave the task running so it resumes on restart.
		m.persist()
		return
	}
	// The user answered quickly: the next leg is queued and takes over, so
	// don't ask or report what it is about to settle.
	handover := l.queued > 0
	t.Updated = time.Now()
	switch {
	case errors.Is(err, ErrOverBudget):
		// Told once for all background work by the budget check; no retry
		// prompt, since retrying before the budget allows would pause again.
		t.Status = Paused
		t.PausedFor = budgetPausedFor
		t.PausedBy = PausedBudget
		m.persist()
		return
	case outOfSteps(err):
		t.Status = Paused
		t.PausedBy = PausedSteps
		m.persist()
		m.say(t.Owner, fmt.Sprintf("%s: I ran out of steps before finishing. %s\nSay \"retry task %s\" and I'll carry on from the board.", t.Title, strings.TrimSpace(err.Error()), t.ID), t.source())
		return
	case err != nil:
		t.Status = Failed
		t.Error = err.Error()
		msg := fmt.Sprintf("%s: I hit a problem and stopped (%s). Say \"retry task %s\" to try again.", t.Title, t.Error, t.ID)
		if errors.Is(err, context.DeadlineExceeded) {
			t.Error = "this stretch of work ran for 30 minutes without finishing"
			msg = fmt.Sprintf("%s: I worked on this for 30 minutes without finishing, so I stopped. Say \"retry task %s\" and I'll carry on from the board.", t.Title, t.ID)
		}
		m.persist()
		m.say(t.Owner, msg, t.source())
		then = m.outcome(t)
		return
	case t.Status == Done:
		m.persist()
		m.say(t.Owner, fmt.Sprintf("%s: done. %s", t.Title, t.Result), t.source())
		then = m.outcome(t)
		return
	case t.Status == WaitingUser:
		if t.AskedAt.IsZero() {
			t.AskedAt = time.Now()
		}
		m.persist()
		if !handover {
			m.say(t.Owner, fmt.Sprintf("%s: %s", t.Title, t.Question), events.Source{Kind: "question", Name: t.Title})
		}
		return
	}
	if m.deps.Pending != nil {
		if n, summary := m.deps.Pending(context.Background(), t.Key); n > 0 {
			t.Status = WaitingApproval
			m.persist()
			if !handover {
				m.say(t.Owner, fmt.Sprintf("%s: I need your OK. %s", t.Title, summary), events.Source{Kind: "approval", Name: t.Title})
			}
			return
		}
	}
	if handover {
		m.persist()
		return
	}
	if opts.midTask && t.openSteps() > 0 {
		// An approval's outcome or the owner's answer, and the model
		// stopped there: one step settled is not the task done.
		m.persist()
		input := "[You stopped without calling task_update finish or ask_user, and your board still has open steps. Carry on from the first open step; call finish once the goal is met (or can't be), or ask_user if you need the user.]\n" + t.brief()
		then = func() { m.drive(ctx, t, input) }
		return
	}
	// The model stopped without finishing or asking: treat its last words as the result.
	t.Status = Done
	t.Result = strings.TrimSpace(out)
	if t.Result == "" {
		t.Result = "Finished."
	}
	m.persist()
	m.say(t.Owner, fmt.Sprintf("%s: %s", t.Title, t.Result), t.source())
	then = m.outcome(t)
}

// outcome is what tells Deps.OnOutcome how t ended, to run once the lock
// is let go; nil when nothing hears. Called with m.mu held.
func (m *Manager) outcome(t *Task) func() {
	if m.deps.OnOutcome == nil {
		return nil
	}
	cp := *t
	return func() { m.deps.OnOutcome(cp, cp.Status) }
}

// say tells the task's owner how it went. src says what it is about, so the
// screen keeps a result in Left for you and shows a question as one.
func (m *Manager) say(chatKey, text string, src events.Source) {
	if m.deps.Notify == nil {
		return
	}
	go func() {
		if err := m.deps.Notify(events.WithSource(context.Background(), src), chatKey, text); err != nil {
			m.deps.Log.Warn("tasks: notify", "err", err)
		}
	}()
}

// Get finds a task by id.
func (m *Manager) Get(id string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	return t, ok
}

// ByKey finds the task that owns a scratch conversation.
func (m *Manager) ByKey(key string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.Key == key {
			return t, true
		}
	}
	return nil, false
}

// WaitingOn returns the task waiting for the owner's answer, if exactly one
// asked recently.
func (m *Manager) WaitingOn(owner string, within time.Duration) *Task {
	return m.WaitingFor(func(o string) bool { return o == owner }, within)
}

// WaitingFor is WaitingOn for tasks whose owner chat matches: the owner's
// next message in a chat that stands in for a task's own (where its
// question was delivered) answers it too.
func (m *Manager) WaitingFor(owner func(string) bool, within time.Duration) *Task {
	m.mu.Lock()
	var waiting []*Task
	var owners []string // a task's owner never changes; owner is asked without the lock held
	for _, t := range m.tasks {
		if t.Status == WaitingUser && time.Since(t.AskedAt) < within {
			waiting, owners = append(waiting, t), append(owners, t.Owner)
		}
	}
	m.mu.Unlock()
	var found *Task
	for i, t := range waiting {
		if owner(owners[i]) {
			if found != nil {
				return nil
			}
			found = t
		}
	}
	return found
}

// Answer gives a task waiting on the owner their answer, however long ago
// it asked, and carries on with the task. answer is what they said, in
// their own words.
func (m *Manager) Answer(ctx context.Context, id, answer string) (Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return Task{}, fmt.Errorf("no task %s", id)
	}
	if t.Status != WaitingUser {
		err := fmt.Errorf("task %s (%s) isn't waiting for an answer: it is %s", id, t.Title, t.Status)
		m.mu.Unlock()
		return Task{}, err
	}
	t.Status = Running // taken: a second answer finds it no longer waiting
	t.Updated = time.Now()
	snap := *t
	m.persist()
	m.mu.Unlock()
	input := fmt.Sprintf("[The user replied to your question (%q): %s]\nContinue the task from your board.", snap.Question, answer)
	m.start(ctx, t, func(c context.Context) (string, error) { return m.deps.Run(c, t, input) }, legOpts{midTask: true})
	return snap, nil
}

// Lapsed tells a task that its request for approval of request (as the
// owner would read it: "send: to shop") went unanswered too long and was
// dropped. A task left with nothing else awaiting approval is set aside, the
// owner hears so once, and "retry task" picks it up again.
func (m *Manager) Lapsed(id, request string) {
	m.LapsedAll([]Lapse{{Task: id, Request: request}})
}

// Lapse is one task's request that went unanswered too long.
type Lapse struct{ Task, Request string }

// LapsedAll is Lapsed for several requests at once (a sweep, say the first
// after an upgrade finds old ones): each chat hears one line, not one per task.
func (m *Manager) LapsedAll(ls []Lapse) {
	m.mu.Lock()
	var aside []Task
	var what []string
	for _, l := range ls {
		t, ok := m.tasks[l.Task]
		if !ok || t.Status != WaitingApproval {
			continue
		}
		if m.deps.Pending != nil {
			if n, _ := m.deps.Pending(context.Background(), t.Key); n > 0 {
				continue // it still waits on another
			}
		}
		t.Status = Paused
		t.PausedFor = fmt.Sprintf("the user didn't answer your request to approve %s in time", l.Request)
		t.PausedBy = PausedLapsed
		t.Notes = append(t.Notes, fmt.Sprintf("no answer about %s; set aside", l.Request))
		t.Updated = time.Now()
		aside, what = append(aside, *t), append(what, l.Request)
	}
	if len(aside) > 0 {
		m.persist()
	}
	m.mu.Unlock()
	var owners []string
	byOwner := map[string][]int{}
	for i, t := range aside {
		if _, ok := byOwner[t.Owner]; !ok {
			owners = append(owners, t.Owner)
		}
		byOwner[t.Owner] = append(byOwner[t.Owner], i)
	}
	for _, o := range owners {
		idx := byOwner[o]
		if len(idx) == 1 {
			t := aside[idx[0]]
			m.say(o, fmt.Sprintf("%s: I asked for your OK (%s) and didn't hear back, so I've set this task aside. Say \"retry task %s\" whenever you want me to pick it up again.", t.Title, what[idx[0]], t.ID), t.source())
			continue
		}
		names := make([]string, len(idx))
		for i, j := range idx {
			names[i] = fmt.Sprintf("%s (%s)", aside[j].Title, aside[j].ID)
		}
		m.say(o, fmt.Sprintf("%d tasks were waiting for your OK and didn't hear back, so I've set them aside: %s. Say \"retry task\" and the id whenever you want me to pick one up again.", len(idx), strings.Join(names, ", ")), events.Source{Kind: "task"})
	}
}

// source is what the task's own news comes from: the task, by its title.
func (t Task) source() events.Source { return events.Source{Kind: "task", Name: t.Title} }

// openSteps counts the steps on the board not yet done.
func (t Task) openSteps() int {
	n := 0
	for _, s := range t.Steps {
		if !s.Done {
			n++
		}
	}
	return n
}

// List returns tasks, open ones first, newest first.
func (m *Manager) List() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := out[i].open(), out[j].open()
		if oi != oj {
			return oi
		}
		return out[i].Updated.After(out[j].Updated)
	})
	return out
}

func (t Task) open() bool {
	return t.Status == Running || t.Status == WaitingApproval || t.Status == WaitingUser
}

// Open reports whether the task is still in progress.
func (t Task) Open() bool { return t.open() }

// Cancel stops a task, including any work it is doing right now.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if err := cancellable(id, t, ok); err != nil {
		return err
	}
	t.Status = Cancelled
	t.Updated = time.Now()
	if l := m.legs[id]; l != nil && l.cancel != nil {
		l.cancel()
	}
	m.persist()
	return nil
}

// KeepKeys lists the conversations of tasks that may still carry on (open,
// paused, failed or cancelled but retryable), whose history must be kept.
func (m *Manager) KeepKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, t := range m.tasks {
		if t.Status != Done {
			out = append(out, t.Key)
		}
	}
	sort.Strings(out)
	return out
}

// Retry restarts a failed, paused or cancelled task from its board.
func (m *Manager) Retry(ctx context.Context, id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no task %s", id)
	}
	if t.open() {
		m.mu.Unlock()
		return fmt.Errorf("task %s is still %s", id, t.Status)
	}
	if t.Status == Done {
		m.mu.Unlock()
		return fmt.Errorf("task %s already finished", id)
	}
	why := "Retrying after a problem"
	switch {
	case t.Status == Paused && t.PausedBy == PausedBudget:
		why = "Carrying on: this was set aside because the monthly model budget was used up, and it has room again"
	case t.Status == Paused && t.PausedFor != "":
		why = "Carrying on: this was set aside because " + t.PausedFor + ", so that wasn't done; ask again if it is still needed"
	case t.Status == Paused:
		why = "Carrying on: you ran out of steps last time"
	}
	t.Status = Running
	t.Error = ""
	t.PausedFor = ""
	t.PausedBy = ""
	t.Updated = time.Now()
	brief := t.brief()
	m.persist()
	m.mu.Unlock()
	m.drive(ctx, t, "["+why+". Continue from the first step on your board that is not done; don't redo finished steps.]\n"+brief)
	return nil
}

// Prune drops finished tasks older than age. A task paused for the budget
// stays until it carries on, which may not be until next month.
func (m *Manager) Prune(age time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.tasks {
		if !t.open() && !(t.Status == Paused && t.PausedBy == PausedBudget) && time.Since(t.Updated) > age {
			delete(m.tasks, id)
			delete(m.legs, id)
		}
	}
	m.persist()
}

// Board renders a task for the user or the model.
func (t Task) Board() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] %s\n", t.ID, t.Status, t.Title)
	for _, s := range t.Steps {
		mark := "☐"
		if s.Done {
			mark = "☑"
		}
		fmt.Fprintf(&b, "  %s %s\n", mark, s.Text)
	}
	if n := len(t.Notes); n > 0 {
		fmt.Fprintf(&b, "  note: %s\n", t.Notes[n-1])
	}
	if t.Question != "" && t.Status == WaitingUser {
		fmt.Fprintf(&b, "  waiting for you: %s\n", t.Question)
	}
	if t.Result != "" {
		fmt.Fprintf(&b, "  result: %s\n", t.Result)
	}
	if t.Error != "" {
		fmt.Fprintf(&b, "  error: %s\n", t.Error)
	}
	return b.String()
}

// Tools returns start_task, task_update, list_tasks and cancel_task.
func (m *Manager) Tools() []tools.Tool {
	return []tools.Tool{
		tools.New("start_task",
			"Start a background task for anything that will take more than a couple of steps or a while (planning a trip, chasing a refund, comparing options across sites, anything with waiting in it). You keep chatting normally; the task works on its own, keeps a board of steps the user can see, and messages them when it needs a decision or is done. Tell the user in one line that you've started it.",
			tools.Schema(map[string]tools.Prop{
				"title": {Type: "string", Description: "Three to six words, e.g. \"Bali trip in March\"", Required: true},
				"goal":  {Type: "string", Description: "What done looks like, with every detail the user gave (dates, budget, names, preferences)", Required: true},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Title, Goal string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				t, err := m.Start(context.WithoutCancel(ctx), call.ChatKey, in.Title, in.Goal)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("started task %s (%s). It runs in the background; the user will hear from it when it needs them or finishes.", t.ID, t.Title), nil
			}),
		tools.New("task_update",
			"Keep the task board current (only inside a task). add_steps puts your plan up; step_done ticks a step by its number (1-based); note records a finding; ask_user pauses the task with a question for the user; finish ends it with the result. You can combine step_done and note in one call.",
			tools.Schema(map[string]tools.Prop{
				"add_steps": {Type: "string", Description: "JSON array of short step strings"},
				"step_done": {Type: "integer", Description: "Number of the step just completed"},
				"note":      {Type: "string", Description: "One line on what you found or decided"},
				"ask_user":  {Type: "string", Description: "A question the user must answer before you can continue; then stop calling tools"},
				"finish":    {Type: "string", Description: "The result for the user, a few lines at most"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				t, ok := m.ByKey(call.ChatKey)
				if !ok {
					return "", errors.New("task_update only works inside a background task; use start_task to begin one")
				}
				var in struct {
					AddSteps string `json:"add_steps"`
					StepDone int    `json:"step_done"`
					Note     string `json:"note"`
					AskUser  string `json:"ask_user"`
					Finish   string `json:"finish"`
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				m.mu.Lock()
				defer m.mu.Unlock()
				if in.AddSteps != "" {
					var steps []string
					if err := json.Unmarshal([]byte(in.AddSteps), &steps); err != nil {
						return "", fmt.Errorf("add_steps must be a JSON array of strings: %w", err)
					}
					for _, s := range steps {
						if s = strings.TrimSpace(s); s != "" {
							t.Steps = append(t.Steps, Step{Text: s})
						}
					}
				}
				if in.StepDone > 0 && in.StepDone <= len(t.Steps) {
					t.Steps[in.StepDone-1].Done = true
				}
				if n := strings.TrimSpace(in.Note); n != "" {
					t.Notes = append(t.Notes, n)
					if len(t.Notes) > 30 {
						t.Notes = t.Notes[len(t.Notes)-30:]
					}
				}
				if q := strings.TrimSpace(in.AskUser); q != "" {
					t.Status = WaitingUser
					t.Question = q
					t.AskedAt = time.Now() // from now the owner's next message is the answer
				}
				if r := strings.TrimSpace(in.Finish); r != "" {
					t.Status = Done
					t.Result = r
					for i := range t.Steps {
						t.Steps[i].Done = true
					}
				}
				t.Updated = time.Now()
				m.persist()
				if t.Status == WaitingUser {
					return "Paused for the user's answer. Stop here; you will be resumed with their reply.", nil
				}
				if t.Status == Done {
					return "Task finished. Stop here.", nil
				}
				return "board updated:\n" + t.Board(), nil
			}),
		tools.New("list_tasks", "Show the background tasks and their boards: what's running, what's waiting on the user, what finished.", tools.Schema(nil), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				list := m.List()
				if len(list) == 0 {
					return "no background tasks", nil
				}
				var b strings.Builder
				b.WriteString(openLine(list))
				for _, t := range list {
					b.WriteString(t.Board())
				}
				return b.String(), nil
			}),
		tools.New("retry_task", "Restart a failed, paused (out of steps) or cancelled background task from where its board left off.", tools.Schema(map[string]tools.Prop{"id": {Type: "string", Required: true}}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if err := m.Retry(context.WithoutCancel(ctx), in.ID); err != nil {
					return "", err
				}
				return "retrying " + in.ID, nil
			}),
		tools.WithSummaryAndCheck(tools.New("cancel_task", "Stop a background task.", tools.Schema(map[string]tools.Prop{"id": {Type: "string", Required: true}}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if err := m.Cancel(in.ID); err != nil {
					return "", err
				}
				return "cancelled " + in.ID, nil
			}), m.cancelSummary, m.cancelCheck),
	}
}
