package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// attachment is a file on a Discord message.
type attachment struct {
	ContentType string  `json:"content_type"`
	URL         string  `json:"url"`
	Size        int64   `json:"size"`
	Duration    float64 `json:"duration_secs"` // voice messages
}

// cdnHosts are where Discord keeps attachments; nothing else is fetched,
// whatever a message claims.
var cdnHosts = []string{"cdn.discordapp.com", "media.discordapp.net"}

// attachment is how to fetch a message's first attachment when the twin
// can take it in (a voice message, a picture), nil otherwise.
func (c *Channel) attachment(m message) *channels.Attachment {
	if len(m.Attachments) == 0 {
		return nil
	}
	a := m.Attachments[0]
	if k := channels.MediaKind(a.ContentType); k != channels.Photo && k != channels.Voice {
		return nil
	}
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" || !slices.Contains(cdnHosts, u.Hostname()) {
		return nil
	}
	return &channels.Attachment{Mime: a.ContentType, Size: a.Size, Seconds: int(a.Duration + 0.5),
		Fetch: func(ctx context.Context, max int64) ([]byte, error) {
			if max > 0 && a.Size > max {
				return nil, channels.ErrTooBig
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
			if err != nil {
				return nil, err
			}
			resp, err := c.http.Do(req)
			if err != nil {
				var ue *url.Error
				if errors.As(err, &ue) {
					err = ue.Err // the signed link is not for the log
				}
				return nil, fmt.Errorf("discord attachment: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("discord attachment: %s", resp.Status)
			}
			return channels.ReadCapped(resp.Body, max)
		}}
}
