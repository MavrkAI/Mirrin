// Package memory is Mirrin's long-term memory: facts about the user, conversation
// history, reminders, pending approvals and the audit log. Everything lives in a
// single local SQLite database.
package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	dir string
	// bmu keeps a backup from being taken half-way through a forget.
	bmu sync.Mutex
	// factsVer changes whenever a fact is added or removed.
	factsVer atomic.Int64
	// forgot hears that a fact was forgotten (OnForgot), and remembered
	// that one was kept (OnRemembered).
	fmu        sync.Mutex
	forgot     []func()
	remembered []func(Fact)
}

// OnForgot adds f to what hears that a fact was forgotten, once it is gone
// from memory and the daily copies: the running twin asks for an encrypted
// backup soon, so the newest snapshot doesn't keep it until the night's.
func (s *Store) OnForgot(f func()) {
	s.fmu.Lock()
	s.forgot = append(s.forgot, f)
	s.fmu.Unlock()
}

// OnRemembered adds f to what hears that a fact was kept, once it is in
// memory: the running twin shows a fact it just learned in a chat on this
// Mac under the reply, with an Undo (daemon/remembered.go).
func (s *Store) OnRemembered(f func(Fact)) {
	s.fmu.Lock()
	s.remembered = append(s.remembered, f)
	s.fmu.Unlock()
}

// Open creates or opens the memory database in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// secure_delete zeroes deleted rows on disk, so what the user asks the twin
	// to forget doesn't linger in free pages.
	dsn := fileURI(filepath.Join(dir, "memory.db")) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: dir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// fileURI is path as an SQLite file: URI. SQLite reads '#' as the start of a
// fragment and '%' as an escape, and the driver splits at the first '?', so
// a data folder with any of them in its name would open the wrong file.
func fileURI(path string) string {
	return "file:" + strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
}

// PersonaVoiceKey is the kv record earlier releases kept of what the persona
// asked of the voice settings when they saved its picks into config.yaml.
// The daemon no longer writes it (the persona's picks are never saved); it
// reads a record left behind to tell those old values apart from the
// owner's, and removes it at the next settings save (daemon/persona.go).
// Whatever rewrites config.yaml from outside the daemon (identity import)
// removes it too.
const PersonaVoiceKey = "persona.voice.v1"

// FileURI is fileURI for other packages that open SQLite files under the
// data folder (identity's export and import, the WhatsApp session).
func FileURI(path string) string { return fileURI(path) }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// migrate checks memory.db and brings its schema up to date: the ordered
// migrations in migrate.go (and any registered elsewhere), then the repairs
// that run on every start.
func (s *Store) migrate() error {
	// The schema, approvals' risk and input hash among it (approvals_migrate.go).
	if err := s.prepare(context.Background()); err != nil {
		return err
	}
	// Approvals written since without an input hash (by an older Mirrin
	// after going back a version, or copied in by an identity import, which
	// doesn't bring the migrations record) get one now, on every open, not
	// only when migration 4 ran.
	if err := hashLegacyApprovals(context.Background(), s.db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Reminders set during a background run used to be keyed to its scratch
	// conversation, which nothing can deliver to. They belong to the chat behind it.
	return s.rescueReminders()
}

// rescueReminders moves unfired reminders off scratch keys onto their live
// chat. A '#' alone doesn't make a key scratch (IRC rooms have one), so
// LiveKey decides row by row.
func (s *Store) rescueReminders() error {
	rows, err := s.db.Query(`SELECT id, chat_key FROM reminders WHERE fired=0 AND instr(chat_key, '#')>0`)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	moves := map[int64]string{}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			rows.Close()
			return fmt.Errorf("migrate: %w", err)
		}
		if live := LiveKey(key); live != key {
			moves[id] = live
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for id, key := range moves {
		if _, err := s.db.Exec(`UPDATE reminders SET chat_key=? WHERE id=?`, key, id); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// reScratch is the suffix background work adds to a chat key: a protocol,
// task, watcher, phone call, follow-up or job runs in "telegram:1#protocol-…",
// maybe nested ("…#task-0927#protocol-…"). The purposes are spelled out
// because a '#' alone proves nothing: an IRC room is "irc:#golang|tony".
var reScratch = regexp.MustCompile(`#(?:protocol|task|watch|phone|call|portrait|patterns|nudge|firstlook|followup)-[\w.-]*(?:#|$)`)

// LiveKey is the conversation the user can see behind a scratch key: anything
// a run in "telegram:1#protocol-…" leaves for the user belongs to "telegram:1".
// Any other key is returned as is.
func LiveKey(chatKey string) string {
	for off := 0; off < len(chatKey); {
		loc := reScratch.FindStringIndex(chatKey[off:])
		if loc == nil {
			break
		}
		// The suffix follows a chat id; it never starts one ("irc:#task-force|tony").
		if i := off + loc[0]; i > 0 && !strings.ContainsRune(": ", rune(chatKey[i-1])) {
			return chatKey[:i]
		}
		off += loc[0] + 1
	}
	return chatKey
}

// IsWatchRun reports whether a chat key is a watcher's run (or work nested
// in one): a turn whose input is what others wrote, an email or an invite.
func IsWatchRun(chatKey string) bool {
	return strings.Contains(chatKey[len(LiveKey(chatKey)):], "#watch-")
}

// IsScratch reports whether a chat key is background work rather than a
// conversation the user can see.
func IsScratch(chatKey string) bool { return LiveKey(chatKey) != chatKey }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ---- Facts -----------------------------------------------------------------

// Fact is one thing Mirrin knows.
type Fact struct {
	ID        int64
	Subject   string
	Content   string
	Source    string
	CreatedAt time.Time
}

// Remember stores a fact. Subject groups related facts ("user", "family", "work").
func (s *Store) Remember(ctx context.Context, subject, content, source string) (int64, error) {
	subject = strings.ToLower(strings.TrimSpace(subject))
	if subject == "" {
		subject = "general"
	}
	content = strings.TrimSpace(content)
	at := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO facts(subject, content, source, created_at, updated_at) VALUES(?,?,?,?,?)`,
		subject, content, source, at, at)
	if err != nil {
		return 0, err
	}
	s.factsVer.Add(1)
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	created, _ := time.Parse(time.RFC3339, at)
	s.fmu.Lock()
	hooks := slices.Clone(s.remembered)
	s.fmu.Unlock()
	for _, h := range hooks {
		h(Fact{ID: id, Subject: subject, Content: content, Source: source, CreatedAt: created})
	}
	return id, nil
}

// FactByID is one fact, or an error wrapping ErrNoFact when id doesn't exist.
func (s *Store) FactByID(ctx context.Context, id int64) (Fact, error) {
	fs, err := s.queryFacts(ctx, `SELECT id, subject, content, source, created_at FROM facts WHERE id=?`, id)
	if err != nil {
		return Fact{}, err
	}
	if len(fs) == 0 {
		return Fact{}, fmt.Errorf("fact #%d: %w", id, ErrNoFact)
	}
	return fs[0], nil
}

// FactsVersion changes whenever a fact is added or removed, so a caller that
// keeps what it read from memory can tell when it is out of date.
func (s *Store) FactsVersion() int64 { return s.factsVer.Load() }

// Recall searches facts by keyword across subject and content: the facts
// matching the most words first, newest first among equals. Little words
// ("my", "the") only count when the query has nothing else, and a fact that
// matches nothing is never returned. An empty query lists the newest facts.
func (s *Store) Recall(ctx context.Context, query string, limit int) ([]Fact, error) {
	if limit <= 0 {
		limit = 20
	}
	if strings.TrimSpace(query) == "" {
		return s.queryFacts(ctx, `SELECT id, subject, content, source, created_at FROM facts ORDER BY id DESC LIMIT ?`, limit)
	}
	all, err := s.queryFacts(ctx, `SELECT id, subject, content, source, created_at FROM facts ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	return ranked(all, searchTerms(query), limit), nil
}

// AllFacts returns every fact, oldest first, capped at limit.
func (s *Store) AllFacts(ctx context.Context, limit int) ([]Fact, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.queryFacts(ctx, `SELECT id, subject, content, source, created_at FROM facts ORDER BY id ASC LIMIT ?`, limit)
}

func (s *Store) queryFacts(ctx context.Context, q string, args ...any) ([]Fact, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		var f Fact
		var created string
		if err := rows.Scan(&f.ID, &f.Subject, &f.Content, &f.Source, &created); err != nil {
			return nil, err
		}
		f.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---- Conversations ----------------------------------------------------------

// AppendMessage stores a message in a conversation.
func (s *Store) AppendMessage(ctx context.Context, chatKey string, m llm.Message) error {
	b, err := json.Marshal(m.Blocks)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO messages(chat_key, role, blocks, created_at) VALUES(?,?,?,?)`,
		chatKey, string(m.Role), string(b), now())
	return err
}

// History returns the last n messages of a conversation in chronological order.
func (s *Store) History(ctx context.Context, chatKey string, n int) ([]llm.Message, error) {
	if n <= 0 {
		n = 40
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, blocks FROM (SELECT id, role, blocks FROM messages WHERE chat_key=? ORDER BY id DESC LIMIT ?) ORDER BY id ASC`,
		chatKey, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []llm.Message
	for rows.Next() {
		var role, blocks string
		if err := rows.Scan(&role, &blocks); err != nil {
			return nil, err
		}
		m := llm.Message{Role: llm.Role(role)}
		if err := json.Unmarshal([]byte(blocks), &m.Blocks); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClearHistory forgets a conversation.
func (s *Store) ClearHistory(ctx context.Context, chatKey string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE chat_key=?`, chatKey)
	return err
}

// MentionedElsewhere reports whether any conversation but chatKey holds
// text (a photo's path, say), so a file two conversations share outlives
// clearing one of them.
func (s *Store) MentionedElsewhere(ctx context.Context, chatKey, text string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat_key<>? AND instr(blocks, ?)>0`, chatKey, text).Scan(&n)
	return n > 0, err
}

// ---- Reminders --------------------------------------------------------------

// Reminder is a timed nudge. In the table, fired is 0 while pending, 1 once
// delivered, 2 when delivery was given up and 3 when it was ticked off
// before it went out. done_at is when the owner ticked it off.
type Reminder struct {
	ID      int64
	ChatKey string
	DueAt   time.Time
	Text    string
	// Attempts counts failed deliveries so far.
	Attempts int
	// Kind is "remind", or "check" for one that looks into something
	// rather than only reminding; Brief is its notes from when it was set.
	Kind  string
	Brief string
	// Snoozes counts the times it was put back for later.
	Snoozes int
	// FiredAt is when it last went out (zero until it has).
	FiredAt time.Time
}

// ErrNoReminder means there is no reminder by that number.
var ErrNoReminder = errors.New("no such reminder")

// reminderCols are the columns queryReminders reads, in order.
const reminderCols = `id, chat_key, due_at, text, attempts, kind, brief, snoozes, fired_at`

// AddReminder schedules a reminder. One set during a background run is kept
// for the chat behind it (see LiveKey), where it can be listed and delivered.
func (s *Store) AddReminder(ctx context.Context, chatKey string, due time.Time, text string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO reminders(chat_key, due_at, text, created_at) VALUES(?,?,?,?)`,
		LiveKey(chatKey), due.UTC().Format(time.RFC3339), text, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// KindCheck is a reminder that checks on something (a follow-up) rather
// than only reminding.
const KindCheck = "check"

// ErrTooManyChecks means max follow-ups are already open.
var ErrTooManyChecks = errors.New("too many follow-ups open")

// AddCheck schedules a follow-up: at due the twin looks into about again,
// with brief, its notes from now. While max are still to run, it adds none
// and says ErrTooManyChecks.
func (s *Store) AddCheck(ctx context.Context, chatKey string, due time.Time, about, brief string, max int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO reminders(chat_key, due_at, text, created_at, kind, brief)
		SELECT ?,?,?,?,?,? WHERE (SELECT count(*) FROM reminders WHERE kind=? AND fired=0) < ?`,
		LiveKey(chatKey), due.UTC().Format(time.RFC3339), about, now(), KindCheck, brief, KindCheck, max)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrTooManyChecks
	}
	return res.LastInsertId()
}

// DueReminders returns unfired reminders due at or before t, leaving out any
// whose next delivery attempt is still to come.
func (s *Store) DueReminders(ctx context.Context, t time.Time) ([]Reminder, error) {
	ts := t.UTC().Format(time.RFC3339)
	return s.queryReminders(ctx, `SELECT `+reminderCols+` FROM reminders WHERE fired=0 AND due_at<=? AND retry_at<=? ORDER BY due_at`, ts, ts)
}

// PendingReminders lists unfired reminders for a chat.
func (s *Store) PendingReminders(ctx context.Context, chatKey string) ([]Reminder, error) {
	return s.queryReminders(ctx, `SELECT `+reminderCols+` FROM reminders WHERE fired=0 AND chat_key=? ORDER BY due_at`, LiveKey(chatKey))
}

// MarkFired records that a reminder was delivered, and when.
func (s *Store) MarkFired(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET fired=1, fired_at=? WHERE id=?`, now(), id)
	return err
}

// MarkDone records that the owner ticked a reminder off. One that hasn't
// gone out yet never will. Ticking one already done changes nothing; one
// that isn't there is ErrNoReminder.
func (s *Store) MarkDone(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE reminders SET done_at=?, fired=CASE fired WHEN 0 THEN 3 ELSE fired END WHERE id=? AND done_at IS NULL`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM reminders WHERE id=?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNoReminder
	}
	return nil
}

// Snooze puts a reminder back for later: due again at due, one more snooze
// counted, and pending again with a fresh set of delivery attempts.
func (s *Store) Snooze(ctx context.Context, id int64, due time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE reminders SET due_at=?, snoozes=snoozes+1, fired=0, attempts=0, retry_at='' WHERE id=? AND done_at IS NULL`,
		due.UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoReminder
	}
	return nil
}

// LastFired is the reminder for a chat that went out most recently, within
// the last within, and isn't ticked off or put back since. chatKey "" is
// any chat's: one set by voice, say, reaches the owner on their phone.
func (s *Store) LastFired(ctx context.Context, chatKey string, within time.Duration) (Reminder, bool) {
	q := `SELECT ` + reminderCols + ` FROM reminders WHERE fired=1 AND done_at IS NULL AND fired_at<>'' AND fired_at>=?`
	args := []any{time.Now().Add(-within).UTC().Format(time.RFC3339)}
	if chatKey != "" {
		q += ` AND chat_key=?`
		args = append(args, LiveKey(chatKey))
	}
	rs, err := s.queryReminders(ctx, q+` ORDER BY fired_at DESC, id DESC LIMIT 1`, args...)
	if err != nil || len(rs) == 0 {
		return Reminder{}, false
	}
	return rs[0], true
}

// RecentlyFired lists reminders that went off within the given time and
// haven't been ticked off, newest first: the twin must not say one "will go
// off at 21:01" after it has.
func (s *Store) RecentlyFired(ctx context.Context, within time.Duration) []Reminder {
	rs, err := s.queryReminders(ctx, `SELECT `+reminderCols+` FROM reminders WHERE fired=1 AND done_at IS NULL AND fired_at<>'' AND fired_at>=? ORDER BY fired_at DESC, id DESC LIMIT 5`,
		time.Now().Add(-within).UTC().Format(time.RFC3339))
	if err != nil {
		return nil
	}
	return rs
}

// RetryReminder records a failed delivery and when to try again.
func (s *Store) RetryReminder(ctx context.Context, id int64, attempts int, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET attempts=?, retry_at=? WHERE id=?`, attempts, at.UTC().Format(time.RFC3339), id)
	return err
}

// GiveUpReminder stops trying to deliver a reminder.
func (s *Store) GiveUpReminder(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET fired=2 WHERE id=?`, id)
	return err
}

// CancelReminder removes an unfired reminder.
func (s *Store) CancelReminder(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM reminders WHERE id=? AND fired=0`, id)
	return err
}

func (s *Store) queryReminders(ctx context.Context, q string, args ...any) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		var due, fired string
		if err := rows.Scan(&r.ID, &r.ChatKey, &due, &r.Text, &r.Attempts, &r.Kind, &r.Brief, &r.Snoozes, &fired); err != nil {
			return nil, err
		}
		r.DueAt, _ = time.Parse(time.RFC3339, due)
		r.FiredAt, _ = time.Parse(time.RFC3339, fired)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Audit ------------------------------------------------------------------

// Audit appends an entry to the audit log. Every tool call, approval and
// outbound message is recorded so the user can always see what Mirrin did.
func (s *Store) Audit(ctx context.Context, kind, chatKey, detail string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit(ts, kind, chat_key, detail) VALUES(?,?,?,?)`, now(), kind, chatKey, detail)
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	TS      time.Time
	Kind    string
	ChatKey string
	Detail  string
}

// RecentAudit returns the newest n audit entries.
func (s *Store) RecentAudit(ctx context.Context, n int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, kind, chat_key, detail FROM audit ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&ts, &e.Kind, &e.ChatKey, &e.Detail); err != nil {
			return nil, err
		}
		e.TS, _ = time.Parse(time.RFC3339, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Key/value --------------------------------------------------------------

// Set stores a small piece of state (for example the owner's default chat).
func (s *Store) Set(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Get reads a piece of state.
func (s *Store) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// ---- Portrait ---------------------------------------------------------------

// Portrait is Mirrin's short written understanding of the user, refreshed weekly.
type Portrait struct {
	Text      string    `json:"text"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GetPortrait returns the current portrait (empty if none).
func (s *Store) GetPortrait(ctx context.Context) (Portrait, error) {
	raw, err := s.Get(ctx, "portrait")
	if err != nil || raw == "" {
		return Portrait{}, err
	}
	var p Portrait
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Portrait{}, err
	}
	return p, nil
}

// SetPortrait stores a new portrait.
func (s *Store) SetPortrait(ctx context.Context, text string) error {
	b, _ := json.Marshal(Portrait{Text: strings.TrimSpace(text), UpdatedAt: time.Now()})
	return s.Set(ctx, "portrait", string(b))
}

// KeyPortraitAside is when the portrait was last set aside because the owner
// asked the twin to forget something (daemon/portrait.go). Writing a new
// portrait clears it.
const KeyPortraitAside = "portrait.aside"

// DeletePortrait removes the portrait, with the earlier one and a proposed
// new one ("portrait.prev", "portrait.new") if they are kept.
func (s *Store) DeletePortrait(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, deletePortrait)
	return err
}

// deletePortrait removes every kept portrait from a memory database: this
// one, a backup or another recorded copy.
const deletePortrait = `DELETE FROM kv WHERE key IN ('portrait', 'portrait.prev', 'portrait.new')`

// PruneScratch deletes messages of scratch conversations (see IsScratch)
// older than age. Conversations in keep are left alone: a background task
// that can still carry on needs its brief and findings however long it waits.
// A live chat with a '#' in its key (an IRC room) is never pruned.
func (s *Store) PruneScratch(ctx context.Context, age time.Duration, keep ...string) (int64, error) {
	cutoff := time.Now().Add(-age).UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT chat_key FROM messages WHERE instr(chat_key, '#')>0 AND created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return 0, err
		}
		if IsScratch(k) && !slices.Contains(keep, k) {
			keys = append(keys, k)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int64
	for _, k := range keys {
		res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE chat_key=? AND created_at < ?`, k, cutoff)
		if err != nil {
			return 0, err
		}
		d, _ := res.RowsAffected()
		n += d
	}
	return n, tx.Commit()
}

// RecentUserRequests returns what the user said in live conversations over the
// last window (scratch conversations excluded, see IsScratch), oldest first.
// A '#' alone doesn't make a conversation scratch: an IRC room is
// "irc:#golang|tony", and what the owner asks there counts.
func (s *Store) RecentUserRequests(ctx context.Context, since time.Duration, limit int) ([]string, error) {
	cutoff := time.Now().Add(-since).UTC().Format(time.RFC3339)
	// SQL drops the commonest background runs (protocols, tasks, watchers)
	// and bounds the scan; IsScratch decides each row that's left.
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_key, blocks FROM messages WHERE role='user' AND created_at >= ?
		 AND chat_key NOT GLOB '*[^: ]#protocol-*' AND chat_key NOT GLOB '*[^: ]#task-*' AND chat_key NOT GLOB '*[^: ]#watch-*'
		 ORDER BY id DESC LIMIT ?`, cutoff, 50*limit+500)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for len(out) < limit && rows.Next() {
		var key, blocks string
		if err := rows.Scan(&key, &blocks); err != nil {
			return nil, err
		}
		if IsScratch(key) {
			continue
		}
		var bs []llm.Block
		if json.Unmarshal([]byte(blocks), &bs) != nil {
			continue
		}
		m := llm.Message{Role: llm.RoleUser, Blocks: bs}
		t := strings.TrimSpace(m.PlainText())
		if t == "" || strings.HasPrefix(t, "[") || strings.HasPrefix(t, "/") {
			continue
		}
		out = append(out, t)
	}
	// oldest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// AllPendingReminders lists unfired reminders across chats, soonest first.
func (s *Store) AllPendingReminders(ctx context.Context, limit int) ([]Reminder, error) {
	if limit <= 0 {
		limit = 10
	}
	return s.queryReminders(ctx, `SELECT `+reminderCols+` FROM reminders WHERE fired=0 ORDER BY due_at LIMIT ?`, limit)
}

// RecentAuditOfKind returns the newest n audit entries of one kind.
func (s *Store) RecentAuditOfKind(ctx context.Context, kind string, n int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, kind, chat_key, detail FROM audit WHERE kind=? ORDER BY id DESC LIMIT ?`, kind, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&ts, &e.Kind, &e.ChatKey, &e.Detail); err != nil {
			return nil, err
		}
		e.TS, _ = time.Parse(time.RFC3339, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
