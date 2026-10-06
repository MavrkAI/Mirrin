package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// ---- Approvals --------------------------------------------------------------

// Approval is a tool call waiting for the user's go-ahead.
type Approval struct {
	ID        int64
	ChatKey   string
	Tool      string
	Input     json.RawMessage
	Summary   string
	Status    string // pending, approved, denied, superseded, expired
	CreatedAt time.Time
	// Risk is what the call could do, as it was when the owner was asked.
	// A request stored before risks were kept counts as dangerous, the most
	// careful handling there is.
	Risk tools.Risk
	// InputHash is the SHA-256 (hex) of Input as it was stored when asked:
	// what the owner says yes to is exactly what runs (see Intact).
	InputHash string
	// DecidedBy says who decided it and how ("the owner (channel, telegram)").
	DecidedBy string
	// ResolvedAt is when it got its outcome; zero while pending.
	ResolvedAt time.Time
}

// HashInput is the hash an approval keeps of its stored input.
func HashInput(input json.RawMessage) string {
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])
}

// Intact reports whether the input is still the one the owner was asked
// about. Forgetting a fact rewrites any request that quoted it, and such a
// request must not then run as if the owner had seen it that way.
func (a Approval) Intact() bool {
	return a.InputHash == "" || HashInput(a.Input) == a.InputHash
}

// riskFrom reads a stored risk. Anything but read or write is dangerous.
func riskFrom(s string) tools.Risk {
	switch s {
	case tools.RiskRead.String():
		return tools.RiskRead
	case tools.RiskWrite.String():
		return tools.RiskWrite
	}
	return tools.RiskDangerous
}

// CreateApproval records a pending tool call and the risk the owner is asked
// at. Left out, the risk counts as dangerous, the most careful handling.
func (s *Store) CreateApproval(ctx context.Context, chatKey, tool string, input json.RawMessage, summary string, risk ...tools.Risk) (int64, error) {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	r := tools.RiskDangerous
	if len(risk) > 0 {
		r = risk[0]
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO approvals(chat_key, tool, input, summary, created_at, risk, input_hash) VALUES(?,?,?,?,?,?,?)`,
		chatKey, tool, string(input), summary, now(), r.String(), HashInput(input))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const approvalCols = `id, chat_key, tool, input, summary, status, created_at, risk, input_hash, decided_by, COALESCE(resolved_at, '')`

type scanner interface{ Scan(dest ...any) error }

func scanApproval(row scanner) (Approval, error) {
	var a Approval
	var input, created, risk, resolved string
	if err := row.Scan(&a.ID, &a.ChatKey, &a.Tool, &input, &a.Summary, &a.Status, &created, &risk, &a.InputHash, &a.DecidedBy, &resolved); err != nil {
		return Approval{}, err
	}
	a.Input = json.RawMessage(input)
	a.CreatedAt, _ = time.Parse(time.RFC3339, created)
	a.ResolvedAt, _ = time.Parse(time.RFC3339, resolved)
	a.Risk = riskFrom(risk)
	return a, nil
}

func (s *Store) queryApprovals(ctx context.Context, where string, args ...any) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApproval fetches an approval by id.
func (s *Store) GetApproval(ctx context.Context, id int64) (*Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// PendingApprovals lists approvals awaiting a decision for a chat.
func (s *Store) PendingApprovals(ctx context.Context, chatKey string) ([]Approval, error) {
	return s.queryApprovals(ctx, `status='pending' AND chat_key=?`, chatKey)
}

// AllPendingApprovals lists approvals awaiting a decision across chats.
func (s *Store) AllPendingApprovals(ctx context.Context) ([]Approval, error) {
	return s.queryApprovals(ctx, `status='pending'`)
}

// LapsedApprovals lists approvals still waiting that were asked before cutoff.
func (s *Store) LapsedApprovals(ctx context.Context, cutoff time.Time) ([]Approval, error) {
	return s.queryApprovals(ctx, `status='pending' AND created_at < ?`, cutoff.UTC().Format(time.RFC3339))
}

// ResolveApproval gives a pending approval its outcome: approved, denied,
// superseded or expired.
func (s *Store) ResolveApproval(ctx context.Context, id int64, status string) error {
	return s.ResolveApprovalBy(ctx, id, status, "")
}

// ResolveApprovalBy is ResolveApproval that also records who decided, and
// how. Only one outcome is ever recorded: of two racing decisions the second
// gets an error.
func (s *Store) ResolveApprovalBy(ctx context.Context, id int64, status, by string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status=?, resolved_at=?, decided_by=? WHERE id=? AND status='pending'`, status, now(), by, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("approval %d is not pending", id)
	}
	return nil
}

// ReviseApproval changes a decided approval's outcome, only if it is still
// from: an approved call that no longer fitted by the time it was due to run
// becomes "expired", as nothing was done.
func (s *Store) ReviseApproval(ctx context.Context, id int64, from, to string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status=?, resolved_at=? WHERE id=? AND status=?`, to, now(), id, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("approval %d is not %s", id, from)
	}
	return nil
}

// Unset removes a piece of state.
func (s *Store) Unset(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, key)
	return err
}
