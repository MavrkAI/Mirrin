package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/llm"
)

// ErrNoFact means there is no fact with the given id.
var ErrNoFact = errors.New("no such fact in memory")

// forgotten stands in for the wording of a forgotten fact.
const forgotten = "[forgotten]"

// Forget deletes a fact by id; see ForgetFact.
func (s *Store) Forget(ctx context.Context, id int64) error {
	_, err := s.ForgetFact(ctx, id)
	return err
}

// ForgetFact deletes a fact and the copies of its wording the twin kept on
// the way: the remember call that stored it (tool input, approval request,
// audit lines) and recall results that listed it, whatever its length. A fact
// long enough to be unmistakable (three words and 16 bytes or more) is also
// redacted where the twin repeated it word for word: its replies, other tool
// calls and results, the audit log. A short one ("blue") is only removed from
// its own trail, so forgetting never garbles unrelated text ("bluetooth").
// The user's own messages are left as they are until the conversation is
// cleared. Deleted rows are zeroed on disk and pushed out of the write-ahead
// log, and the backups are scrubbed the same way. It returns the fact that
// was removed, or an error wrapping ErrNoFact when id doesn't exist.
func (s *Store) ForgetFact(ctx context.Context, id int64) (Fact, error) {
	fs, err := s.queryFacts(ctx, `SELECT id, subject, content, source, created_at FROM facts WHERE id=?`, id)
	if err != nil {
		return Fact{}, err
	}
	if len(fs) == 0 {
		return Fact{}, fmt.Errorf("fact #%d: %w", id, ErrNoFact)
	}
	f := fs[0]
	s.bmu.Lock()
	defer s.bmu.Unlock()
	if err := scrub(ctx, s.db, f); err != nil {
		return Fact{}, err
	}
	s.factsVer.Add(1)
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	s.scrubBackups(ctx, f)
	s.fmu.Lock()
	hooks := slices.Clone(s.forgot)
	s.fmu.Unlock()
	for _, h := range hooks {
		h()
	}
	return f, nil
}

// scrub removes a fact and redacts its wording in one database (see ForgetFact).
func scrub(ctx context.Context, db *sql.DB, f Fact) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM facts WHERE id=? AND content=?`, f.ID, f.Content); err != nil {
		return err
	}
	t := newTrail(f)
	if len(t.needles) == 0 {
		return tx.Commit()
	}
	// Only rows holding (the start of) the fact can need a change.
	where := func(cols ...string) (string, []any) {
		var ors []string
		var args []any
		for _, c := range cols {
			for _, n := range t.needles {
				ors = append(ors, "instr("+c+", ?)>0")
				args = append(args, prefix(n, minRedact))
			}
		}
		return strings.Join(ors, " OR "), args
	}

	// The audit log keeps the start of every tool input, cut at a few hundred characters.
	cond, args := where("detail")
	if err := rewrite(ctx, tx, `SELECT id, detail FROM audit WHERE `+cond, args, `UPDATE audit SET detail=? WHERE id=?`,
		func(cols []string) string { return t.line(cols[0]) }); err != nil {
		return err
	}
	// A sensitive fact waited for approval with its full text.
	cond, args = where("input", "summary")
	rows, err := tx.QueryContext(ctx, `SELECT id, input, summary FROM approvals WHERE `+cond, args...)
	if err != nil {
		return err
	}
	type approval struct {
		id             int64
		input, summary string
	}
	var aps []approval
	for rows.Next() {
		var a approval
		if err := rows.Scan(&a.id, &a.input, &a.summary); err != nil {
			rows.Close()
			return err
		}
		aps = append(aps, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range aps {
		input, summary := string(t.input(json.RawMessage(a.input))), t.line(a.summary)
		if input == a.input && summary == a.summary {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approvals SET input=?, summary=? WHERE id=?`, input, summary, a.id); err != nil {
			return err
		}
	}
	// Conversations hold the remember call itself, recall results and the
	// twin's own replies.
	cond, args = where("blocks")
	if err := rewrite(ctx, tx, `SELECT id, role, blocks FROM messages WHERE `+cond, args, `UPDATE messages SET blocks=? WHERE id=?`,
		func(cols []string) string {
			role, raw := llm.Role(cols[0]), cols[1]
			var bs []llm.Block
			if json.Unmarshal([]byte(raw), &bs) != nil {
				return raw
			}
			changed := false
			for i, b := range bs {
				switch b.Type {
				case llm.BlockToolUse:
					bs[i].Input = t.input(b.Input)
				case llm.BlockToolResult:
					bs[i].Text = t.result(b.Text)
				default:
					if role != llm.RoleUser { // the user's own words stay
						bs[i].Text = t.text(b.Text)
					}
				}
				changed = changed || bs[i].Text != b.Text || string(bs[i].Input) != string(b.Input)
			}
			out, err := json.Marshal(bs)
			if !changed || err != nil {
				return raw
			}
			return string(out)
		}); err != nil {
		return err
	}
	return tx.Commit()
}

// rewrite runs fix over the rows sel finds (an id, then text columns) and
// writes back, with upd(value, id), each last column that fix changes.
func rewrite(ctx context.Context, tx *sql.Tx, sel string, args []any, upd string, fix func(cols []string) string) error {
	rows, err := tx.QueryContext(ctx, sel, args...)
	if err != nil {
		return err
	}
	n, err := rows.Columns()
	if err != nil {
		rows.Close()
		return err
	}
	type change struct {
		id  int64
		val string
	}
	var todo []change
	for rows.Next() {
		var id int64
		cols := make([]string, len(n)-1)
		dest := []any{&id}
		for i := range cols {
			dest = append(dest, &cols[i])
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return err
		}
		if v := fix(cols); v != cols[len(cols)-1] {
			todo = append(todo, change{id, v})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range todo {
		if _, err := tx.ExecContext(ctx, upd, c.val, c.id); err != nil {
			return err
		}
	}
	return nil
}

// minRedact is the shortest leading piece of a fact recognised in a log line
// that was cut short.
const minRedact = 24

// trail is what a forget looks for: the fact's wordings, and whether it is
// long enough that a word-for-word copy anywhere can only be this fact.
type trail struct {
	f       Fact
	needles []string
	loose   bool
}

func newTrail(f Fact) trail {
	c := strings.TrimSpace(f.Content)
	return trail{f: f, needles: wordings(c), loose: len(c) >= 16 && len(strings.Fields(c)) >= 3}
}

// text redacts free text the twin wrote: only an unmistakable fact, and only
// where it stands as words of its own.
func (t trail) text(s string) string {
	if !t.loose || s == "" {
		return s
	}
	for _, n := range t.needles {
		s = replaceWords(s, n)
	}
	for _, n := range t.needles {
		s = redactCut(s, n)
	}
	return s
}

// result redacts a tool result: recall's line for the fact, then free text.
func (t trail) result(s string) string {
	listed := fmt.Sprintf("#%d [%s] ", t.f.ID, t.f.Subject)
	s = strings.ReplaceAll(s, listed+t.f.Content, listed+forgotten)
	return t.text(s)
}

var reFactField = regexp.MustCompile(`"fact"\s*:\s*"|\bfact=`)

// line redacts an audit line or an approval summary. The fact is removed
// where it is the "fact" of a logged remember call (JSON, perhaps cut short)
// or the fact= of a summary; the rest is treated as free text.
func (t trail) line(s string) string {
	var b strings.Builder
	for {
		loc := reFactField.FindStringIndex(s)
		if loc == nil {
			break
		}
		b.WriteString(s[:loc[1]])
		ends := ",)" // a summary: "remember_sensitive(fact=…, subject=health)"
		if s[loc[0]] == '"' {
			ends = `"`
		}
		s = s[loc[1]:]
		if n := t.valueLen(s, ends); n > 0 {
			b.WriteString(forgotten)
			s = s[n:]
		}
	}
	b.WriteString(s)
	return t.text(b.String())
}

// valueLen is the length of the fact at the start of s: a wording followed
// by one of ends or the end of s, or at least minRedact bytes of one cut
// short by truncation ("…").
func (t trail) valueLen(s, ends string) int {
	for _, n := range t.needles {
		if strings.HasPrefix(s, n) && (len(s) == len(n) || strings.IndexByte(ends, s[len(n)]) >= 0) {
			return len(n)
		}
	}
	if i := strings.Index(s, "…"); i >= minRedact {
		for _, n := range t.needles {
			if strings.HasPrefix(n, s[:i]) {
				return i
			}
		}
	}
	return 0
}

// input redacts a tool call's JSON: the "fact" of the remember call that
// stored this fact, whatever its length, and other strings as free text.
func (t trail) input(raw json.RawMessage) json.RawMessage {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		out := t.line(string(raw))
		if out == string(raw) {
			return raw
		}
		if json.Valid([]byte(out)) {
			return json.RawMessage(out)
		}
		return json.RawMessage("{}")
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return t.text(x)
		case map[string]any:
			for k, y := range x {
				if s, ok := y.(string); ok && k == "fact" && strings.TrimSpace(s) == t.f.Content {
					x[k] = forgotten
					continue
				}
				x[k] = walk(y)
			}
		case []any:
			for i, y := range x {
				x[i] = walk(y)
			}
		}
		return v
	}
	before, _ := json.Marshal(v)
	after, err := json.Marshal(walk(v))
	if err != nil || string(after) == string(before) {
		return raw // untouched: keep the model's own formatting
	}
	return after
}

// wordings are the forms a fact's text takes when stored: as is, and escaped
// inside JSON (with and without HTML escaping).
func wordings(content string) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	out := []string{content}
	add := func(s string) {
		for _, o := range out {
			if o == s {
				return
			}
		}
		out = append(out, s)
	}
	if b, err := json.Marshal(content); err == nil {
		add(string(b[1 : len(b)-1]))
	}
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if enc.Encode(content) == nil {
		s := strings.TrimSpace(sb.String())
		add(s[1 : len(s)-1])
	}
	return out
}

// replaceWords replaces each copy of n in s that stands apart from the text
// around it: "blue" goes from "a blue car" but not from "bluetooth".
func replaceWords(s, n string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, n)
		if i < 0 {
			break
		}
		if apart(s[:i], n, s[i+len(n):]) {
			b.WriteString(s[:i])
			b.WriteString(forgotten)
			s = s[i+len(n):]
			continue
		}
		b.WriteString(s[:i+1])
		s = s[i+1:]
	}
	b.WriteString(s)
	return b.String()
}

// redactCut replaces the start of n (at least minRedact bytes) where a copy
// was cut short: followed by "…" or the end of s.
func redactCut(s, n string) string {
	if len(n) <= minRedact {
		return s
	}
	head := prefix(n, minRedact)
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], head)
		if i < 0 {
			break
		}
		i += from
		l := len(head)
		for l < len(n) && i+l < len(s) && s[i+l] == n[l] {
			l++
		}
		rest := s[i+l:]
		if l < len(n) && (rest == "" || strings.HasPrefix(rest, "…")) && apart(s[:i], n, "") {
			s = s[:i] + forgotten + rest
			from = i + len(forgotten)
			continue
		}
		from = i + 1
	}
	return s
}

// apart reports whether n, found between before and after, is not part of a
// longer word on either side.
func apart(before, n, after string) bool {
	first, _ := utf8.DecodeRuneInString(n)
	last, _ := utf8.DecodeLastRuneInString(n)
	prev, _ := utf8.DecodeLastRuneInString(before)
	next, _ := utf8.DecodeRuneInString(after)
	return (before == "" || !isWordRune(first) || !isWordRune(prev)) &&
		(after == "" || !isWordRune(last) || !isWordRune(next))
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// prefix is s cut to at most n bytes on a character boundary.
func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// scrubBackups forgets the fact in every backup too, and in the other copies
// recorded with NoteCopy (what an import replaced). A backup that can't be
// cleaned is deleted rather than left holding what the user asked to forget.
// A copy's portrait goes too, as the twin sets the live one aside
// (daemon/portrait.go): it may say the same thing in other words, and
// restoring the copy would bring it straight back.
func (s *Store) scrubBackups(ctx context.Context, f Fact) {
	paths, _ := s.Backups()
	for _, p := range paths {
		if err := scrubFile(ctx, p, f); err != nil {
			_ = os.Remove(p)
		}
	}
	s.scrubCopies(ctx, f)
}

func scrubFile(ctx context.Context, path string, f Fact) error {
	db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=busy_timeout(5000)&_pragma=secure_delete(1)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := scrub(ctx, db, f); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, deletePortrait); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}
