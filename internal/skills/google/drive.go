package google

import (
	"context"
	"fmt"
	"io"
	"strings"

	"google.golang.org/api/drive/v3"

	"github.com/MavrkAI/Mirrin/internal/skills/email/mailtext"
	"github.com/MavrkAI/Mirrin/internal/skills/web"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func (a *Auth) drive(ctx context.Context) (*drive.Service, error) {
	opts, err := a.ClientOptions()
	if err != nil {
		return nil, a.Explain("drive", err)
	}
	return drive.NewService(ctx, opts...)
}

// DriveTools returns drive_search and drive_read.
func (a *Auth) DriveTools() []tools.Tool {
	return []tools.Tool{
		tools.New("drive_search", "Find files in Google Drive by name or content. Returns id, name, type and last modified.",
			tools.Schema(map[string]tools.Prop{
				"query": {Type: "string", Description: "Words to look for (name or full text)", Required: true},
				"max":   {Type: "integer", Description: "default 10"},
			}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct {
					Query string
					Max   int64
				}
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if in.Max <= 0 || in.Max > 30 {
					in.Max = 10
				}
				svc, err := a.drive(ctx)
				if err != nil {
					return "", err
				}
				q := strings.ReplaceAll(in.Query, "'", "\\'")
				res, err := svc.Files.List().Q(fmt.Sprintf("(name contains '%s' or fullText contains '%s') and trashed = false", q, q)).
					PageSize(in.Max).OrderBy("modifiedTime desc").Fields("files(id,name,mimeType,modifiedTime,webViewLink)").Context(ctx).Do()
				if err != nil {
					return "", a.Explain("drive", err)
				}
				if len(res.Files) == 0 {
					return "nothing found", nil
				}
				var b strings.Builder
				for _, f := range res.Files {
					fmt.Fprintf(&b, "id=%s | %s | %s | %s\n", f.Id, f.Name, kind(f.MimeType), f.ModifiedTime)
				}
				return b.String(), nil
			}),
		tools.New("drive_read", "Read a Drive file as text: Google Docs, Sheets (CSV) and Slides are exported; text, markdown, CSV, JSON and HTML files are read directly.",
			tools.Schema(map[string]tools.Prop{"id": {Type: "string", Required: true}}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ ID string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				svc, err := a.drive(ctx)
				if err != nil {
					return "", err
				}
				f, err := svc.Files.Get(in.ID).Fields("id,name,mimeType").Context(ctx).Do()
				if err != nil {
					return "", a.Explain("drive", err)
				}
				var rc io.ReadCloser
				switch f.MimeType {
				case "application/vnd.google-apps.document":
					resp, err := svc.Files.Export(in.ID, "text/plain").Context(ctx).Download()
					if err != nil {
						return "", a.Explain("drive", err)
					}
					rc = resp.Body
				case "application/vnd.google-apps.spreadsheet":
					resp, err := svc.Files.Export(in.ID, "text/csv").Context(ctx).Download()
					if err != nil {
						return "", a.Explain("drive", err)
					}
					rc = resp.Body
				case "application/vnd.google-apps.presentation":
					resp, err := svc.Files.Export(in.ID, "text/plain").Context(ctx).Download()
					if err != nil {
						return "", a.Explain("drive", err)
					}
					rc = resp.Body
				default:
					if !strings.HasPrefix(f.MimeType, "text/") && f.MimeType != "application/json" {
						return "", fmt.Errorf("%s is %s; I can only read text-like files and Google Docs/Sheets/Slides", f.Name, f.MimeType)
					}
					resp, err := svc.Files.Get(in.ID).Context(ctx).Download()
					if err != nil {
						return "", a.Explain("drive", err)
					}
					rc = resp.Body
				}
				defer rc.Close()
				data, _ := io.ReadAll(io.LimitReader(rc, 2<<20))
				text := string(data)
				if f.MimeType == "text/html" {
					text = web.HTMLToText(text)
				}
				text = mailtext.Truncate(text, 30000)
				return fmt.Sprintf("%s (%s)\n\n%s", f.Name, kind(f.MimeType), text), nil
			}),
	}
}

func kind(mime string) string {
	switch mime {
	case "application/vnd.google-apps.document":
		return "Google Doc"
	case "application/vnd.google-apps.spreadsheet":
		return "Google Sheet"
	case "application/vnd.google-apps.presentation":
		return "Google Slides"
	case "application/vnd.google-apps.folder":
		return "folder"
	}
	return mime
}
