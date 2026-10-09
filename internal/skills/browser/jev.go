package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Jev Ultrafast (browser-use × TypeSafe) is a small, fast browsing agent
// that drives a Chrome over its debugging port: given a page and a goal it
// clicks and types its way there in seconds, no model turn per click. When
// a checkout of it is on this computer, the twin gets browser_run and hands
// it whole goals ("search for X, stop when the results show") instead of
// driving every click itself with browser_act.
//
// It drives the twin's own Chrome: the same profile, so the same logins,
// and the same guard, so a page can't steer it anywhere private. The twin's
// Chrome listens on a debugging port of its own only when Jev is here. Jev
// never pays, sends or submits anything personal for the twin: those stay
// with browser_act, where the owner sees each step.

// jevRunner runs Jev in its checkout against the Chrome at cdp (an http
// DevTools address) with the runner script on stdin, and returns what it
// printed. Tests replace it.
type jevRunner func(ctx context.Context, dir, cdp string, args []string) ([]byte, error)

// jevDir finds the Jev checkout: the configured one, or ~/jev-ultrafast.
// "" means there isn't one; "off" in the config keeps browser_run away.
func jevDir(cfg string) string {
	if strings.EqualFold(strings.TrimSpace(cfg), "off") {
		return ""
	}
	dir := strings.TrimSpace(cfg)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, "jev-ultrafast")
	} else if strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[2:])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err != nil {
		return ""
	}
	return dir
}

// jevTools returns browser_run when Jev is on this computer, else nothing.
func (s *Session) jevTools() []tools.Tool {
	dir := jevDir(s.cfg.JevDir)
	if dir == "" {
		return nil
	}
	if s.jevRun == nil {
		s.jevRun = runJev
	}
	if s.jevCDP == nil {
		s.jevCDP = s.chromeCDP
	}
	return []tools.Tool{
		tools.New("browser_run",
			"Hand a whole browsing goal to Jev, a fast agent that drives the twin's browser (same logins as browse_page): it opens the URL and clicks, types and scrolls through the site until the goal is reached, in seconds, and returns the page it landed on. Use it for multi-step work on a website: searching, filtering, finding a listing, getting to a result or detail page. Give one narrow goal with a clear stop (\"Search for flats in Fitzroy under $600 a week. Stop when the results list is visible\"); repeat goals for an ordered sequence. Put concrete values in the goal: it never invents personal details. Never use it to pay, book, send a message or submit a form with personal data: do those with browser_act so the user approves each step. If it comes back BLOCKED (a captcha, a pop-up tab, an iframe, a file upload), carry on with browse_page and browser_act.",
			tools.Schema(map[string]tools.Prop{
				"url":   {Type: "string", Description: "Where to start, an absolute URL", Required: true},
				"goals": {Type: "string", Description: "What to get done, with a stop condition; or a JSON array of goals done in order", Required: true},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				defer s.using(call.ChatKey)()
				var in jevInput
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				goals, err := in.list()
				if err != nil {
					return "", err
				}
				u, err := s.webURL(ctx, in.URL)
				if err != nil {
					return "", err
				}
				cdp, err := s.jevCDP(ctx)
				if err != nil {
					return "", err
				}
				args := []string{"--url", u}
				for _, g := range goals {
					args = append(args, "--goal", g)
				}
				rctx, cancel := context.WithTimeout(ctx, jevTimeout)
				defer cancel()
				out, err := s.jevRun(rctx, dir, cdp, args)
				if err != nil {
					return "", jevError(err, out)
				}
				return formatJev(out)
			}).WithRiskFor(jevRisk),
	}
}

// jevDebugPort is the port the twin's Chrome listens on for Jev, chosen
// free once per session, or 0 when Jev isn't here and Chrome keeps its
// usual private one. startChrome asks (browser.go).
func (s *Session) jevDebugPort() int {
	if jevDir(s.cfg.JevDir) == "" {
		return 0
	}
	s.jevPortOnce.Do(func() {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return
		}
		s.jevPort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	})
	return s.jevPort
}

// chromeCDP makes sure the twin's Chrome is up and says where Jev reaches it.
func (s *Session) chromeCDP(ctx context.Context) (string, error) {
	if _, err := s.tab(false); err != nil {
		return "", err
	}
	port := s.jevDebugPort()
	if port == 0 {
		return "", errors.New("the twin's browser isn't listening for Jev")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port), nil
}

// jevTimeout bounds one run: Jev takes up to 60 steps at about a quarter
// second each, a couple of seconds for a typed field.
const jevTimeout = 3 * time.Minute

// jevInput is browser_run's input.
type jevInput struct {
	URL   string
	Goals string
}

// list returns the goals in order: a JSON array, or the one goal as given.
func (in jevInput) list() ([]string, error) {
	raw := strings.TrimSpace(in.Goals)
	if raw == "" {
		return nil, errors.New("browser_run needs a goal: what to get done, and when to stop")
	}
	if strings.HasPrefix(raw, "[") {
		var goals []string
		if err := json.Unmarshal([]byte(raw), &goals); err == nil {
			var kept []string
			for _, g := range goals {
				if g = strings.TrimSpace(g); g != "" {
					kept = append(kept, g)
				}
			}
			if len(kept) == 0 {
				return nil, errors.New("browser_run needs a goal: what to get done, and when to stop")
			}
			return kept, nil
		}
	}
	return []string{raw}, nil
}

// jevRisk raises browser_run to dangerous when the goal reads like a
// payment or a purchase, so the approvals floor always asks.
func jevRisk(ctx context.Context, call tools.Call) tools.Risk {
	var in jevInput
	if tools.Decode(call, &in) != nil {
		return tools.RiskWrite
	}
	if rePayment.MatchString(in.Goals) {
		return tools.RiskDangerous
	}
	return tools.RiskWrite
}

// runJev runs the runner script in the checkout through uv, which brings
// the checkout's Python, its packages and the keys in its .env.
func runJev(ctx context.Context, dir, cdp string, args []string) ([]byte, error) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return nil, errors.New("Jev needs uv (https://docs.astral.sh/uv/) installed to run")
	}
	cmd := exec.CommandContext(ctx, uv, append([]string{"run", "--env-file", ".env", "python", "-"}, args...)...)
	cmd.Dir = dir
	// Its own Browser Harness daemon, on the twin's Chrome, not the owner's.
	cmd.Env = append(os.Environ(), "BU_NAME=mirrin", "BU_CDP_URL="+cdp)
	cmd.Stdin = strings.NewReader(jevRunnerPy)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), errors.New("Jev ran out of time (3 minutes) before reaching the goal")
		}
		return stdout.Bytes(), fmt.Errorf("Jev stopped: %s", lastLines(stderr.String(), 3))
	}
	return stdout.Bytes(), nil
}

// lastLines keeps the tail of a log, where the reason usually is.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

// jevError is a run that didn't finish, said plainly.
func jevError(err error, out []byte) error {
	msg := err.Error()
	if strings.Contains(msg, "unreachable") || strings.Contains(msg, "daemon") || strings.Contains(msg, "connection") {
		return fmt.Errorf("%s. Jev couldn't reach the twin's browser: carry on with browse_page and browser_act", msg)
	}
	return errors.New(msg)
}

// jevSummary is what the runner prints.
type jevSummary struct {
	Status    string `json:"status"`
	ElapsedMS int    `json:"elapsed_ms"`
	URL       string `json:"final_url"`
	Title     string `json:"final_title"`
	Actions   []struct {
		Step        int    `json:"step"`
		Operation   string `json:"operation"`
		Action      string `json:"action"`
		Text        string `json:"text"`
		PageChanged *bool  `json:"page_changed"`
	} `json:"actions"`
	Text string `json:"page_text"`
}

// formatJev turns the runner's JSON into the page as the twin reads pages,
// with the steps Jev took and what to do next.
func formatJev(out []byte) (string, error) {
	start := bytes.IndexByte(out, '{')
	if start < 0 {
		return "", errors.New("Jev finished without saying what it reached")
	}
	var sum jevSummary
	if err := json.Unmarshal(out[start:], &sum); err != nil {
		return "", errors.New("Jev finished without saying what it reached")
	}
	var b strings.Builder
	status := strings.ToUpper(sum.Status)
	fmt.Fprintf(&b, "jev: %s after %d steps in %.1fs\n", status, len(sum.Actions), float64(sum.ElapsedMS)/1000)
	switch status {
	case "DONE":
		b.WriteString("Jev says the goal is reached: check the page below before telling the user it is.\n")
	case "BLOCKED":
		b.WriteString("Jev couldn't go on (a captcha, pop-up, iframe or upload): carry on from this page with browse_page and browser_act.\n")
	default:
		b.WriteString("The goal wasn't reached: look at the page below, then try a narrower goal or carry on with browser_act.\n")
	}
	fmt.Fprintf(&b, "page: %s\nurl: %s\n", sum.Title, sum.URL)
	b.WriteString("Jev worked in its own tab of the twin's browser, signed in the same; browse_page with the url carries on from here.\n")
	if len(sum.Actions) > 0 {
		b.WriteString("\nSTEPS:\n")
		for _, a := range sum.Actions {
			fmt.Fprintf(&b, "%d. %s %q", a.Step, a.Operation, a.Action)
			if a.Text != "" {
				fmt.Fprintf(&b, " typed %q", a.Text)
			}
			b.WriteString("\n")
		}
	}
	text := strings.TrimSpace(sum.Text)
	if len(text) > 12000 {
		text = text[:12000] + "\n…[truncated]"
	}
	b.WriteString("\nTEXT:\n" + text)
	return b.String(), nil
}

// jevRunnerPy runs one Jev task in the checkout and prints a JSON summary.
// It goes to python on stdin, so nothing has to be installed beside Jev.
const jevRunnerPy = `import argparse, json, sys
from jev_ultrafast import Agent

p = argparse.ArgumentParser()
p.add_argument("--url", required=True)
p.add_argument("--goal", action="append", required=True)
a = p.parse_args()

state = None
with Agent(a.url, a.goal) as agent:
    for state in agent.run():
        print(f"{state['elapsed_ms']:>6} ms  {len(state['history'])} actions  {state['status']}", file=sys.stderr)

page = (state or {}).get("page") or {}
print(json.dumps({
    "status": (state or {}).get("status"),
    "elapsed_ms": (state or {}).get("elapsed_ms"),
    "final_url": page.get("url"),
    "final_title": page.get("title"),
    "actions": [
        {k: h.get(k) for k in ("step", "operation", "action", "text", "page_changed")}
        for h in (state or {}).get("history", [])
    ],
    "page_text": (page.get("text") or "")[:12000],
}))
`
