package memory

import (
	"context"
	"strings"
)

// FactsPage returns one page of facts, oldest first, and how many there are
// in all. A query keeps the facts whose subject or content contains it
// (ignoring case), and the total counts only those.
func (s *Store) FactsPage(ctx context.Context, offset, limit int, query string) ([]Fact, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	where, args := "", []any{}
	if q := strings.TrimSpace(query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
		where = ` WHERE lower(subject) LIKE ? ESCAPE '\' OR lower(content) LIKE ? ESCAPE '\'`
		args = append(args, like, like)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM facts`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	facts, err := s.queryFacts(ctx, `SELECT id, subject, content, source, created_at FROM facts`+where+` ORDER BY id ASC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	return facts, total, nil
}
