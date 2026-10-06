package memory

import "context"

// The approvals table keeps what it needs to hold a yes to what was asked
// (migrate.go runs this once, inside the migration transaction):
//
//   - risk: what the call could do, as the owner was asked. Rows from before
//     it was kept count as dangerous, the most careful handling, so nothing
//     already waiting is ever treated as harmless by mistake.
//   - input_hash: SHA-256 of the input as stored, so what runs is what the
//     owner said yes to. Older rows get the hash of the input they hold now,
//     here and again on every open (store.go migrate), so one written later
//     without it (by an older Mirrin after going back a version, or copied
//     in by an identity import) gets it too; until then it counts as
//     unchanged (Approval.Intact).
//   - decided_by: who gave the outcome, and how.
func init() {
	Register(Migration{Version: 4, Name: "approvals-risk", Up: migrateApprovals})
}

func migrateApprovals(ctx context.Context, db Execer) error {
	for _, c := range []struct{ name, decl string }{
		{"risk", "TEXT NOT NULL DEFAULT 'dangerous'"},
		{"input_hash", "TEXT NOT NULL DEFAULT ''"},
		{"decided_by", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := AddColumn(ctx, db, "approvals", c.name, c.decl); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS approvals_waiting ON approvals(status, created_at)`); err != nil {
		return err
	}
	return hashLegacyApprovals(ctx, db)
}

// hashLegacyApprovals fills in the input hash of rows stored before it was kept.
func hashLegacyApprovals(ctx context.Context, db Execer) error {
	rows, err := db.QueryContext(ctx, `SELECT id, input FROM approvals WHERE input_hash=''`)
	if err != nil {
		return err
	}
	hashes := map[int64]string{}
	for rows.Next() {
		var id int64
		var input string
		if err := rows.Scan(&id, &input); err != nil {
			rows.Close()
			return err
		}
		hashes[id] = HashInput([]byte(input))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, h := range hashes {
		if _, err := db.ExecContext(ctx, `UPDATE approvals SET input_hash=? WHERE id=?`, h, id); err != nil {
			return err
		}
	}
	return nil
}
