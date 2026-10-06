package mattermost

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

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
	req.Header.Set("Authorization", "Bearer "+c.token)
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
		return nil, fmt.Errorf("mattermost file: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, channels.ErrTooBig
	}
	return channels.ReadCapped(resp.Body, max)
}

type fileInfo struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

func (c *Channel) attachment(ctx context.Context, p post) *channels.Attachment {
	var f fileInfo
	if len(p.Metadata.Files) > 0 {
		f = p.Metadata.Files[0]
	}
	if f.ID == "" && len(p.FileIDs) > 0 {
		f.ID = p.FileIDs[0]
	}
	if f.ID == "" || strings.ContainsAny(f.ID, "/\\?#") || f.ID == "." || f.ID == ".." {
		return nil
	}
	path := "/files/" + url.PathEscape(f.ID)
	// Some websocket posts include only file_ids. Resolve the small metadata
	// record to classify the file; its bytes are fetched later by the daemon.
	if f.MimeType == "" {
		var info fileInfo
		infoCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := c.api(infoCtx, http.MethodGet, path+"/info", nil, &info); err == nil {
			f.MimeType, f.Size = info.MimeType, info.Size
		}
	}
	return &channels.Attachment{Mime: f.MimeType, Size: f.Size, Fetch: func(ctx context.Context, max int64) ([]byte, error) {
		return c.download(ctx, c.url+"/api/v4"+path, f.Size, max)
	}}
}
