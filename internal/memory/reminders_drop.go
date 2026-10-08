package memory

import "context"

// DropReminder removes a reminder whether or not it has gone out: one that
// was set from a fact goes with the fact when it is forgotten
// (daemon/datereminder.go), wording and all. Unlike CancelReminder, which
// leaves a delivered one in the record.
func (s *Store) DropReminder(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM reminders WHERE id=?`, id)
	return err
}
