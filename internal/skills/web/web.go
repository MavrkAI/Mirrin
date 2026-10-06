// Package web fetches pages as readable text.
package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

var (
	reScript = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)[^>]*>.*?</(script|style|noscript|svg|head)>`)
	reTags   = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpace  = regexp.MustCompile(`[ \t\r\f\v]+`)
	reLines  = regexp.MustCompile(`\n{3,}`)
)

// Tools returns fetch_url. It reaches only the public internet; allow names
// hosts, addresses or ranges on the owner's own network it may also reach
// (skills.web.allow_hosts), such as a home server.
func Tools(allow ...string) []tools.Tool {
	client := newEgress(allow).client()
	return []tools.Tool{
		tools.New("fetch_url",
			"Fetch a web page or API URL and return its readable text (HTML stripped). Use for looking things up, reading articles, checking a site.",
			tools.Schema(map[string]tools.Prop{
				"url":       {Type: "string", Description: "Absolute http(s) URL", Required: true},
				"max_chars": {Type: "integer", Description: "Maximum characters to return (default 12000)"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					URL      string
					MaxChars int `json:"max_chars"`
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
					return "", fmt.Errorf("url must start with http:// or https://")
				}
				if in.MaxChars <= 0 {
					in.MaxChars = 12000
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
				if err != nil {
					return "", err
				}
				req.Header.Set("User-Agent", "Mirrin (+https://github.com/MavrkAI/Mirrin)")
				resp, err := client.Do(req)
				if err != nil {
					var refused *refusedError
					if errors.As(err, &refused) {
						return "", refused
					}
					return "", err
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				if err != nil {
					return "", err
				}
				text := string(body)
				if strings.Contains(resp.Header.Get("Content-Type"), "html") {
					text = HTMLToText(text)
				}
				if len(text) > in.MaxChars {
					text = text[:in.MaxChars] + "\n…[truncated]"
				}
				return fmt.Sprintf("HTTP %d\n\n%s", resp.StatusCode, text), nil
			}),
	}
}

// HTMLToText crudely strips markup.
func HTMLToText(s string) string {
	s = reScript.ReplaceAllString(s, " ")
	s = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>|</h[1-6]>|</tr>`).ReplaceAllString(s, "\n")
	s = reTags.ReplaceAllString(s, " ")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	s = reSpace.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = reLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
