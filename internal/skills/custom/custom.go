// Package custom lets the twin extend itself: small script tools written by
// the user or by the twin (with approval) live under ~/.mirrin/tools, one
// folder each with a tool.yaml manifest and a script. They load at start and
// can be created, replaced or removed live. This is how the long tail gets
// covered without waiting for someone to write Go.
package custom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MavrkAI/Mirrin/internal/procenv"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Manifest is tool.yaml.
type Manifest struct {
	Name        string         `yaml:"name" json:"name"`
	Description string         `yaml:"description" json:"description"`
	Risk        string         `yaml:"risk" json:"risk"` // read | write | dangerous
	Command     string         `yaml:"command" json:"command"`
	Args        map[string]Arg `yaml:"args" json:"args"`
	// Env names the environment variables the script may see (an API key the
	// owner set). Nothing else from the daemon's environment is passed.
	Env     []string `yaml:"env,omitempty" json:"env,omitempty"`
	Author  string   `yaml:"author,omitempty" json:"author,omitempty"`
	Created string   `yaml:"created,omitempty" json:"created,omitempty"`
}

// twinAuthor marks tools the twin wrote for itself with create_tool.
const twinAuthor = "twin"

// risk is what each run of the tool counts as. A tool the twin wrote never
// runs below write, whatever it declared: a model that can mark its own code
// "read" could give itself a way round every approval. The owner can still
// let one run on its own with autonomy.always_allow.
func (m Manifest) risk() tools.Risk {
	r := riskOf(m.Risk)
	if m.Author == twinAuthor && r < tools.RiskWrite {
		return tools.RiskWrite
	}
	return r
}

// Arg is one input the script accepts.
type Arg struct {
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description" json:"description"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

var reName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,40}$`)

// Store manages the tools directory and keeps the registry in sync.
type Store struct {
	dir string
	reg *tools.Registry
	mu  sync.Mutex
}

// New builds a store over dir (created on demand).
func New(dir string, reg *tools.Registry) *Store {
	return &Store{dir: dir, reg: reg}
}

// LoadAll registers every valid tool under the directory.
func (s *Store) LoadAll() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	var errs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := readManifest(filepath.Join(s.dir, e.Name()))
		if err != nil {
			errs = append(errs, e.Name()+": "+err.Error())
			continue
		}
		s.reg.Register(s.tool(m, filepath.Join(s.dir, e.Name())))
		names = append(names, m.Name)
	}
	sort.Strings(names)
	if len(errs) > 0 {
		return names, errors.New(strings.Join(errs, "; "))
	}
	return names, nil
}

func readManifest(dir string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(dir, "tool.yaml"))
	if err != nil {
		return m, err
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if !reName.MatchString(m.Name) {
		return m, fmt.Errorf("bad name %q (lowercase letters, digits, underscores)", m.Name)
	}
	if m.Command == "" {
		return m, errors.New("command missing")
	}
	switch m.Risk {
	case "read", "write", "dangerous":
	default:
		m.Risk = "write"
	}
	return m, nil
}

func riskOf(s string) tools.Risk {
	switch s {
	case "read":
		return tools.RiskRead
	case "dangerous":
		return tools.RiskDangerous
	}
	return tools.RiskWrite
}

// tool wraps a manifest as a runnable tool: the script gets the JSON input on
// stdin and each argument as ARG_<NAME> in a minimal environment.
func (s *Store) tool(m Manifest, dir string) tools.Tool {
	props := map[string]tools.Prop{}
	for k, a := range m.Args {
		t := a.Type
		if t == "" {
			t = "string"
		}
		props[k] = tools.Prop{Type: t, Description: a.Description, Required: a.Required}
	}
	return tools.New(m.Name, m.Description+" (custom tool)", tools.Schema(props), m.risk(),
		func(ctx context.Context, call tools.Call) (string, error) {
			var in map[string]any
			if len(call.Input) > 0 {
				if err := json.Unmarshal(call.Input, &in); err != nil {
					return "", err
				}
			}
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.CommandContext(ctx, "cmd", "/C", m.Command)
			} else {
				cmd = exec.CommandContext(ctx, "/bin/sh", "-c", m.Command)
			}
			cmd.Dir = dir
			cmd.Env = procenv.With(m.Env...)
			for k, v := range in {
				var sv string
				switch t := v.(type) {
				case string:
					sv = t
				default:
					b, _ := json.Marshal(t)
					sv = string(b)
				}
				cmd.Env = append(cmd.Env, "ARG_"+strings.ToUpper(k)+"="+sv)
			}
			cmd.Stdin = bytes.NewReader(call.Input)
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			err := cmd.Run()
			res := strings.TrimSpace(out.String())
			if len(res) > 60000 {
				res = res[:60000] + "\n…[truncated]"
			}
			if err != nil {
				msg := strings.TrimSpace(errb.String())
				if msg == "" {
					msg = err.Error()
				}
				if res != "" {
					return "", fmt.Errorf("%s\n%s", msg, res)
				}
				return "", errors.New(msg)
			}
			return res, nil
		})
}

// Create writes a tool (manifest + script), smoke-runs it when testInput is
// given, and registers it. language picks the interpreter for the script.
func (s *Store) Create(ctx context.Context, m Manifest, language, code string, testInput map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !reName.MatchString(m.Name) {
		return "", fmt.Errorf("bad name %q (lowercase letters, digits, underscores)", m.Name)
	}
	if strings.TrimSpace(code) == "" {
		return "", errors.New("code is empty")
	}
	if _, taken := s.reg.Get(m.Name); taken {
		if _, err := readManifest(filepath.Join(s.dir, m.Name)); err != nil {
			return "", fmt.Errorf("%q is a built-in tool; pick another name", m.Name)
		}
	}
	var file, run string
	switch strings.ToLower(language) {
	case "python", "python3", "py":
		file, run = "tool.py", "python3 tool.py"
	case "node", "javascript", "js":
		file, run = "tool.js", "node tool.js"
	case "bash", "sh", "shell", "":
		file, run = "tool.sh", "sh tool.sh"
	default:
		return "", fmt.Errorf("language %q not supported (python, node, sh)", language)
	}
	dir := filepath.Join(s.dir, m.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(code), 0o700); err != nil {
		return "", err
	}
	m.Command = run
	if m.Created == "" {
		m.Created = time.Now().Format(time.RFC3339)
	}
	if m.Risk == "" {
		m.Risk = "write"
	}
	data, _ := yaml.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), data, 0o600); err != nil {
		return "", err
	}
	t := s.tool(m, dir)
	result := "created " + m.Name
	if testInput != nil {
		in, _ := json.Marshal(testInput)
		out, err := t.Run(ctx, tools.Call{Input: in})
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("the test run failed, so the tool was not kept: %v", err)
		}
		if len(out) > 1500 {
			out = out[:1500] + "…"
		}
		result += "; test run output:\n" + out
	}
	s.reg.Register(t)
	return result, nil
}

// Remove deletes a custom tool.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, name)
	if _, err := readManifest(dir); err != nil {
		return fmt.Errorf("%q is not a custom tool", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	s.reg.Unregister(name)
	return nil
}

// List returns the manifests of installed custom tools.
func (s *Store) List() []Manifest {
	entries, _ := os.ReadDir(s.dir)
	var out []Manifest
	for _, e := range entries {
		if m, err := readManifest(filepath.Join(s.dir, e.Name())); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// Tools returns create_tool, remove_tool and list_custom_tools.
func (s *Store) Tools() []tools.Tool {
	return []tools.Tool{
		tools.WithSummary(tools.New("create_tool",
			"Write a new tool for yourself when none exists for a job that a short script can do (call an API with a key the user gives you, wrap a command-line program, read a file format). The script receives the arguments as environment variables ARG_<NAME> and the JSON input on stdin, prints its result to stdout, and exits non-zero on failure. It sees only a minimal environment plus the variables you name in env, so name any API key it reads. Keep it small and dependency-free (curl, python3 standard library, node built-ins). The user approves the code; every run counts as at least a write action, so it asks first unless the user has allowed the tool.",
			tools.Schema(map[string]tools.Prop{
				"name":        {Type: "string", Description: "snake_case, e.g. parcel_status", Required: true},
				"description": {Type: "string", Description: "One sentence: what it does and when to use it", Required: true},
				"risk":        {Type: "string", Description: "write (the default) or dangerous (hard to undo: deletes, payments, messages to other people)"},
				"language":    {Type: "string", Description: "python, node or sh", Required: true},
				"code":        {Type: "string", Description: "The complete script", Required: true},
				"args":        {Type: "string", Description: "JSON object of arguments: {\"id\":{\"type\":\"string\",\"description\":\"…\",\"required\":true}}"},
				"env":         {Type: "string", Description: "Comma-separated names of environment variables the script reads, e.g. PARCEL_API_KEY"},
				"test_input":  {Type: "string", Description: "Optional JSON input to run once as a smoke test; the tool is only kept if it succeeds"},
			}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in createInput
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				m := Manifest{Name: in.Name, Description: in.Description, Risk: in.Risk, Env: envNames(in.Env), Author: twinAuthor}
				m.Risk = m.risk().String() // tool.yaml says what actually applies
				if in.Args != "" {
					if err := json.Unmarshal([]byte(in.Args), &m.Args); err != nil {
						return "", fmt.Errorf("args must be a JSON object: %w", err)
					}
				}
				var test map[string]any
				if strings.TrimSpace(in.TestInput) != "" {
					if err := json.Unmarshal([]byte(in.TestInput), &test); err != nil {
						return "", fmt.Errorf("test_input must be a JSON object: %w", err)
					}
				}
				return s.Create(ctx, m, in.Language, in.Code, test)
			}), createSummary),
		tools.New("remove_tool", "Delete a custom tool you or the user created earlier.",
			tools.Schema(map[string]tools.Prop{"name": {Type: "string", Required: true}}), tools.RiskWrite,
			func(ctx context.Context, call tools.Call) (string, error) {
				var in struct{ Name string }
				if err := tools.Decode(call, &in); err != nil {
					return "", err
				}
				if err := s.Remove(in.Name); err != nil {
					return "", err
				}
				return "removed " + in.Name, nil
			}),
		tools.New("list_custom_tools", "List the custom tools that exist, with their descriptions and risk.", tools.Schema(nil), tools.RiskRead,
			func(ctx context.Context, call tools.Call) (string, error) {
				ms := s.List()
				if len(ms) == 0 {
					return "no custom tools yet", nil
				}
				var b strings.Builder
				for _, m := range ms {
					fmt.Fprintf(&b, "%s (%s): %s\n", m.Name, m.risk(), m.Description)
				}
				return b.String(), nil
			}),
	}
}

// createInput is what create_tool takes.
type createInput struct {
	Name, Description, Risk, Language, Code, Args, Env string
	TestInput                                          string `json:"test_input"`
}

// createSummary is the approval for create_tool: the whole script, what it
// can see and how each run will be treated. The owner is agreeing to run
// this code, so they get to read all of it.
func createSummary(call tools.Call) string {
	var in createInput
	_ = tools.Decode(call, &in)
	m := Manifest{Name: in.Name, Risk: in.Risk, Author: twinAuthor}
	lang := strings.TrimSpace(in.Language)
	if lang == "" {
		lang = "sh"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "create_tool %s (%s script): %s\n", in.Name, lang, strings.TrimSpace(in.Description))
	fmt.Fprintf(&b, "Each run counts as %s.\n", m.risk())
	if env := envNames(in.Env); len(env) > 0 {
		fmt.Fprintf(&b, "It can read these environment variables: %s.\n", strings.Join(env, ", "))
	} else {
		b.WriteString("It can't see any keys or tokens from the environment.\n")
	}
	b.WriteString("Code:\n")
	b.WriteString(strings.TrimRight(in.Code, "\n "))
	return b.String()
}

var reEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envNames parses "A, B" into valid variable names.
func envNames(s string) []string {
	var out []string
	for _, n := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if n = strings.TrimPrefix(strings.TrimSpace(n), "$"); reEnvName.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}
