package imessage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// A cut-down Messages chat.db with the columns the channel reads.
const schema = `
CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT);
CREATE TABLE message (ROWID INTEGER PRIMARY KEY, text TEXT, attributedBody BLOB, handle_id INTEGER,
	is_from_me INTEGER, associated_message_type INTEGER, item_type INTEGER, cache_roomnames TEXT,
	cache_has_attachments INTEGER, is_audio_message INTEGER, balloon_bundle_id TEXT);
CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY, mime_type TEXT, transfer_name TEXT, filename TEXT,
	transfer_state INTEGER, total_bytes INTEGER);
CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
INSERT INTO handle VALUES (1, '+61400000002');
`

func TestRowsOnlyDirectMessagesReachTheTwin(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		insert   string
		wantOK   bool
		wantText string
		wantKind string
	}{
		{"plain text", `INSERT INTO message VALUES (1, 'remind me at six', NULL, 1, 0, 0, 0, NULL, 0, 0, NULL)`, true, "remind me at six", ""},
		{"tapback on the twin's reply", `INSERT INTO message VALUES (2, 'Loved “Done, see you at six.”', NULL, 1, 0, 2000, 0, NULL, 0, 0, NULL)`, false, "", ""},
		{"removed tapback", `INSERT INTO message VALUES (3, 'Removed a heart from “Done.”', NULL, 1, 0, 3000, 0, NULL, 0, 0, NULL)`, false, "", ""},
		{"own message", `INSERT INTO message VALUES (4, 'on my way', NULL, 1, 1, 0, 0, NULL, 0, 0, NULL)`, false, "", ""},
		{"group chat", `INSERT INTO message VALUES (5, 'who is in for dinner?', NULL, 1, 0, 0, 0, 'chat123456', 0, 0, NULL)`, false, "", ""},
		{"group renamed", `INSERT INTO message VALUES (6, NULL, NULL, 1, 0, 0, 2, NULL, 0, 0, NULL)`, false, "", ""},
		{"photo only", `INSERT INTO message VALUES (7, char(65532), NULL, 1, 0, 0, 0, NULL, 1, 0, NULL);
			INSERT INTO attachment (ROWID, mime_type, transfer_name, filename) VALUES (1, 'image/jpeg', 'IMG_0001.jpeg', NULL); INSERT INTO message_attachment_join VALUES (7, 1)`, true, "", channels.Photo},
		{"audio message", `INSERT INTO message VALUES (8, char(65532), NULL, 1, 0, 0, 0, NULL, 1, 1, NULL)`, true, "", channels.Voice},
		{"text with a photo", `INSERT INTO message VALUES (9, 'is this mould?' || char(65532), NULL, 1, 0, 0, 0, NULL, 1, 0, NULL);
			INSERT INTO attachment (ROWID, mime_type, transfer_name, filename) VALUES (2, 'image/png', 'IMG_0002.png', NULL); INSERT INTO message_attachment_join VALUES (9, 2)`, true,
			"is this mould?", channels.Photo},
		{"older schema leaves columns empty", `INSERT INTO message VALUES (10, 'still works', NULL, 1, 0, NULL, NULL, NULL, NULL, NULL, NULL)`, true, "still works", ""},
		// A link gets a preview stored as an attachment; it is still just text.
		{"link with its preview", `INSERT INTO message VALUES (11, 'summarise https://example.com/post', NULL, 1, 0, 0, 0, NULL, 1, 0, 'com.apple.messages.URLBalloonProvider');
			INSERT INTO attachment (ROWID, mime_type, transfer_name, filename) VALUES (3, NULL, 'E5A1.pluginPayloadAttachment', NULL); INSERT INTO message_attachment_join VALUES (11, 3)`, true,
			"summarise https://example.com/post", ""},
		{"preview without the balloon", `INSERT INTO message VALUES (12, 'look at https://example.com', NULL, 1, 0, 0, 0, NULL, 1, 0, NULL);
			INSERT INTO attachment (ROWID, mime_type, transfer_name, filename) VALUES (4, NULL, NULL, '~/Library/Messages/Attachments/ab/E5A2.pluginPayloadAttachment'); INSERT INTO message_attachment_join VALUES (12, 4)`, true,
			"look at https://example.com", ""},
		{"a pdf", `INSERT INTO message VALUES (13, char(65532), NULL, 1, 0, 0, 0, NULL, 1, 0, NULL);
			INSERT INTO attachment (ROWID, mime_type, transfer_name, filename) VALUES (5, 'application/pdf', 'lease.pdf', NULL); INSERT INTO message_attachment_join VALUES (13, 5)`, true, "", channels.File},
	}
	var last int64
	for _, tt := range tests {
		if _, err := db.Exec(tt.insert); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		rows, err := poll(context.Background(), db, last)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: poll %v %+v", tt.name, err, rows)
		}
		last = rows[0].ID
		text, kind, ok := rows[0].message()
		if ok != tt.wantOK || text != tt.wantText || kind != tt.wantKind {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, %v)", tt.name, text, kind, ok, tt.wantText, tt.wantKind, tt.wantOK)
		}
		if ok && rows[0].Handle != "+61400000002" {
			t.Errorf("%s: handle %q", tt.name, rows[0].Handle)
		}
	}
}
