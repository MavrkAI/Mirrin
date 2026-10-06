package signal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

var ErrAttachmentUpgrade = errors.New("signal-cli needs updating to fetch attachments")
var ErrAttachmentSizeUnknown = errors.New("signal-cli did not report the attachment size")

type signalAttachment struct {
	ContentType string `json:"contentType"`
	ID          string `json:"id"`
	Size        int64  `json:"size"`
	VoiceNote   bool   `json:"isVoiceNote"`
}

// getAttachment works with both a local subprocess and a remote signal-cli
// daemon. filename is a sender-supplied name, never a local path to open.
// https://github.com/AsamK/signal-cli/blob/master/src/main/java/org/asamk/signal/commands/GetAttachmentCommand.java
func (c *Channel) attachment(files []signalAttachment, sender string) *channels.Attachment {
	if len(files) == 0 || files[0].ID == "" {
		return nil
	}
	f := files[0]
	return &channels.Attachment{Mime: f.ContentType, Size: f.Size, Fetch: func(ctx context.Context, max int64) ([]byte, error) {
		if max <= 0 || max > 25<<20 {
			max = 25 << 20
		}
		if f.Size <= 0 && c.httpAddr == "" {
			return nil, ErrAttachmentSizeUnknown
		}
		if f.Size > max {
			return nil, channels.ErrTooBig
		}
		res, err := c.rpc(ctx, "getAttachment", map[string]any{"account": c.account, "id": f.ID, "recipient": sender})
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "method not found") {
				return nil, ErrAttachmentUpgrade
			}
			return nil, err
		}
		var data struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(res, &data); err != nil {
			return nil, err
		}
		return channels.ReadCapped(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data.Data)), max)
	}}
}
