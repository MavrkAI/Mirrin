package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

var ErrFilePermission = errors.New("Slack needs the files:read permission")

// download keeps credentials on the original origin, including on redirects.
// The hard limit also applies when a caller has not supplied a smaller cap.
func (c *Channel) download(ctx context.Context, address string, size, max int64) ([]byte, error) {
	if max <= 0 || max > 25<<20 {
		max = 25 << 20
	}
	if size > max {
		return nil, channels.ErrTooBig
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)
	client := *c.http
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many media redirects")
		}
		if next.URL.Scheme != req.URL.Scheme || next.URL.Host != req.URL.Host {
			next.Header.Del("Authorization")
			if next.URL.Scheme != "https" {
				return fmt.Errorf("unsafe media redirect")
			}
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack file: HTTP %d", resp.StatusCode)
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return nil, ErrFilePermission
	}
	if resp.ContentLength > max {
		return nil, channels.ErrTooBig
	}
	data, err := channels.ReadCapped(resp.Body, max)
	if err != nil {
		return nil, err
	}
	if strings.Contains(http.DetectContentType(data), "text/html") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(string(data))), "<!doctype html") {
		return nil, ErrFilePermission
	}
	return data, nil
}

// Slack's private file URLs require the bot token and files:read scope.
// https://docs.slack.dev/reference/objects/file-object/
type slackFile struct {
	Mimetype    string `json:"mimetype"`
	URL         string `json:"url_private"`
	DownloadURL string `json:"url_private_download"`
	Size        int64  `json:"size"`
	DurationMS  int    `json:"duration_ms"`
}

func (c *Channel) attachment(files []slackFile) *channels.Attachment {
	if len(files) == 0 {
		return nil
	}
	f := files[0]
	address := f.URL
	if address == "" {
		address = f.DownloadURL
	}
	if address == "" {
		return nil
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Hostname() != "slack.com" && !strings.HasSuffix(u.Hostname(), ".slack.com")) {
		return nil
	}
	return &channels.Attachment{Mime: f.Mimetype, Size: f.Size, Seconds: (f.DurationMS + 999) / 1000, Fetch: func(ctx context.Context, max int64) ([]byte, error) {
		return c.download(ctx, address, f.Size, max)
	}}
}
