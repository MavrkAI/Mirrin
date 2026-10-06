// Package system gives Mirrin hands on the local machine: files and a shell.
// Shell and writes are gated by the approvals policy.
package system

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Tools returns file and shell tools. private names Mirrin's own files kept
// outside its home (data directory, Google token files); like keys and
// browser profiles, touching them always needs the owner's yes.
func Tools(cfg config.System, private ...string) []tools.Tool {
	g := newGuard(private)
	allowed := func(p string) (string, error) {
		abs, err := filepath.Abs(expand(p))
		if err != nil {
			return "", err
		}
		if len(cfg.AllowedDirs) == 0 {
			return abs, nil
		}
		for _, d := range cfg.AllowedDirs {
			if rel, err := filepath.Rel(d, abs); err == nil && !strings.HasPrefix(rel, "..") {
				return abs, nil
			}
		}
		return "", fmt.Errorf("%s is outside the allowed directories", abs)
	}

	ts := []tools.Tool{
		g.gate(tools.New("read_file", "Read a text file from the user's machine.",
			tools.Schema(map[string]tools.Prop{"path": {Type: "string", Description: "File path (~ allowed)", Required: true}}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Path string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				p, err := allowed(in.Path)
				if err != nil {
					return "", err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return "", err
				}
				if len(b) > 100_000 {
					b = append(b[:100_000], []byte("\n…[truncated]")...)
				}
				return string(b), nil
			})),
		g.gate(tools.New("list_dir", "List a directory on the user's machine.",
			tools.Schema(map[string]tools.Prop{"path": {Type: "string", Description: "Directory path (~ allowed)", Required: true}}), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Path string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				p, err := allowed(in.Path)
				if err != nil {
					return "", err
				}
				entries, err := os.ReadDir(p)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, e := range entries {
					if e.IsDir() {
						fmt.Fprintf(&b, "%s/\n", e.Name())
					} else {
						fmt.Fprintf(&b, "%s\n", e.Name())
					}
				}
				return b.String(), nil
			})),
		g.gate(tools.New("write_file", "Create or overwrite a text file on the user's machine.",
			tools.Schema(map[string]tools.Prop{
				"path":    {Type: "string", Description: "File path (~ allowed)", Required: true},
				"content": {Type: "string", Description: "Full file content", Required: true},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Path, Content string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				p, err := allowed(in.Path)
				if err != nil {
					return "", err
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return "", err
				}
				if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), p), nil
			})),
	}
	if cfg.AllowShell {
		ts = append(ts, tools.WithSummaryAndCheck(tools.New("run_shell",
			"Run a shell command on the user's machine and return its output. Dangerous: always goes through approval unless the user has allowed it.",
			tools.Schema(map[string]tools.Prop{
				"command": {Type: "string", Description: "The command line to run", Required: true},
				"cwd":     {Type: "string", Description: "Working directory (default: home)"},
			}), tools.RiskDangerous,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Command, Cwd string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				var cmd *exec.Cmd
				if runtime.GOOS == "windows" {
					cmd = exec.CommandContext(ctx, "cmd", "/C", in.Command)
				} else {
					cmd = exec.CommandContext(ctx, "/bin/sh", "-c", in.Command)
				}
				if in.Cwd != "" {
					cmd.Dir = expand(in.Cwd)
				} else if home, err := os.UserHomeDir(); err == nil {
					cmd.Dir = home
				}
				out, err := cmd.CombinedOutput()
				s := string(out)
				if len(s) > 50_000 {
					s = s[:50_000] + "\n…[truncated]"
				}
				if err != nil {
					return fmt.Sprintf("%s\n(exit: %v)", s, err), nil
				}
				return s, nil
			}), shellSummary, func(_ context.Context, call tools.Call) error {
			var in struct{ Command string }
			if tools.Decode(call, &in) == nil && g.mentionsOwn(in.Command) {
				return errOwnFiles
			}
			return nil
		}))
	}
	return ts
}

// shellSummary is run_shell's approval: the whole command and where it runs,
// since whatever hides past a clipped first line would run too.
func shellSummary(call tools.Call) string {
	var in struct{ Command, Cwd string }
	_ = tools.Decode(call, &in)
	where := in.Cwd
	if where == "" {
		where = "your home folder"
	}
	return fmt.Sprintf("run_shell in %s:\n%s", where, in.Command)
}

func expand(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
