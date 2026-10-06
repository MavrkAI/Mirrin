package imessage

import (
	"context"
	"database/sql"
	"encoding/binary"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// row is one new line in the Messages database.
type row struct {
	ID          int64
	Text        string
	Body        []byte // attributedBody, where recent macOS keeps the text
	Handle      string
	FromMe      bool
	Associated  int64  // associated_message_type: tapbacks, reactions and stickers on another message
	ItemType    int64  // non-zero for renames, joins and other chat events
	Room        string // cache_roomnames: set for group chats
	Attachments bool   // something the sender attached, not a link preview
	Audio       bool
	Filename    string // local attachment path supplied by Messages
	Mime        string // the first attachment's type
	// AttachmentID and Transfer are the first attachment's row and how far
	// Messages had got downloading it when the row was read; transfer
	// reads that again while the file is fetched.
	AttachmentID int64
	Transfer     transfer
	recheck      func(context.Context) (transfer, error)
}

// transfer is Messages' own record of an attachment's download
// (attachment.transfer_state and total_bytes).
type transfer struct {
	State int64
	Total int64
}

// transferFinished is the transfer_state of a completely downloaded file.
const transferFinished = 5

// known reports whether Messages recorded anything about the download;
// when it didn't, the file's size is all there is to go on.
func (t transfer) known() bool { return t.State != 0 || t.Total != 0 }

// done reports whether a file of size bytes is the whole download.
func (t transfer) done(size int) bool {
	return size > 0 && t.State == transferFinished && (t.Total <= 0 || int64(size) == t.Total)
}

// transferOf reads an attachment's download state.
func transferOf(ctx context.Context, db *sql.DB, id int64) (transfer, error) {
	var t transfer
	err := db.QueryRowContext(ctx, `SELECT COALESCE(transfer_state,0), COALESCE(total_bytes,0) FROM attachment WHERE ROWID = ?`, id).Scan(&t.State, &t.Total)
	return t, err
}

// newRows reads the messages after ROWID last, oldest first. A link's
// preview is stored as a .pluginPayloadAttachment (and the message carries
// the URL balloon), so it isn't counted as something attached: "summarise
// https://…" is plain text.
const newRows = `
	SELECT m.ROWID, COALESCE(m.text,''), COALESCE(m.attributedBody, X''), COALESCE(h.id,''), m.is_from_me,
		COALESCE(m.associated_message_type,0), COALESCE(m.item_type,0), COALESCE(m.cache_roomnames,''),
		COALESCE(m.cache_has_attachments,0) AND COALESCE(m.balloon_bundle_id,'') != 'com.apple.messages.URLBalloonProvider'
			AND EXISTS (SELECT 1 FROM message_attachment_join j JOIN attachment a ON a.ROWID = j.attachment_id
				WHERE j.message_id = m.ROWID AND COALESCE(a.transfer_name, a.filename, '') NOT LIKE '%.pluginPayloadAttachment'),
		COALESCE(m.is_audio_message,0),
		COALESCE(a.mime_type,''), COALESCE(a.filename,''),
		COALESCE(a.ROWID,0), COALESCE(a.transfer_state,0), COALESCE(a.total_bytes,0)
	FROM message m LEFT JOIN handle h ON m.handle_id = h.ROWID
	LEFT JOIN attachment a ON a.ROWID = (SELECT j.attachment_id FROM message_attachment_join j JOIN attachment x ON x.ROWID = j.attachment_id
		WHERE j.message_id = m.ROWID AND COALESCE(x.transfer_name, x.filename, '') NOT LIKE '%.pluginPayloadAttachment' LIMIT 1)
	WHERE m.ROWID > ? ORDER BY m.ROWID`

// poll returns the rows after last.
func poll(ctx context.Context, db *sql.DB, last int64) ([]row, error) {
	rows, err := db.QueryContext(ctx, newRows, last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		var fromMe, att, audio int64
		if err := rows.Scan(&r.ID, &r.Text, &r.Body, &r.Handle, &fromMe, &r.Associated, &r.ItemType, &r.Room, &att, &audio, &r.Mime, &r.Filename,
			&r.AttachmentID, &r.Transfer.State, &r.Transfer.Total); err != nil {
			continue
		}
		r.FromMe, r.Attachments, r.Audio = fromMe == 1, att == 1, audio == 1
		if id := r.AttachmentID; id != 0 {
			r.recheck = func(ctx context.Context) (transfer, error) { return transferOf(ctx, db, id) }
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// message says what a row means for the twin: the text of a direct message
// and the kind of anything attached to it. ok is false for everything that
// isn't someone writing to the twin directly: its own messages, tapbacks
// ("Loved “…”"), group chats and chat events.
func (r row) message() (text, kind string, ok bool) {
	if r.FromMe || r.Associated != 0 || r.ItemType != 0 || r.Room != "" {
		return "", "", false
	}
	text = r.Text
	if text == "" {
		text = ExtractText(r.Body)
	}
	// An attachment's place in the text is marked with U+FFFC.
	text = strings.TrimSpace(strings.ReplaceAll(text, "￼", ""))
	if r.Attachments || r.Audio {
		switch {
		case r.Audio:
			kind = channels.Voice
		case r.Mime != "":
			kind = channels.MediaKind(r.Mime)
		default:
			kind = channels.File
		}
	}
	if text == "" && kind == "" {
		return "", "", false
	}
	return text, kind, true
}

// ExtractText pulls the plain string out of an NSAttributedString typedstream
// blob, which is how recent macOS versions store message text.
func ExtractText(b []byte) string {
	i := strings.Index(string(b), "NSString")
	if i < 0 {
		return ""
	}
	p := i + len("NSString")
	// Skip the class-info bytes up to the '+' that precedes the length.
	for p < len(b) && b[p] != '+' {
		p++
	}
	p++
	if p >= len(b) {
		return ""
	}
	var n int
	switch {
	case b[p] == 0x81 && p+2 < len(b):
		n = int(binary.LittleEndian.Uint16(b[p+1 : p+3]))
		p += 3
	case b[p] == 0x82 && p+4 < len(b):
		n = int(binary.LittleEndian.Uint32(b[p+1 : p+5]))
		p += 5
	default:
		n = int(b[p])
		p++
	}
	if n <= 0 || p+n > len(b) {
		return ""
	}
	return string(b[p : p+n])
}
