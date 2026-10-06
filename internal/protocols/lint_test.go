package protocols

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLint(t *testing.T) {
	dir := t.TempDir()
	good := "name: commute check\ndescription: d\nversion: 0.1.0\nauthor: a\nschedule: \"45 7 * * 1-5\"\nrequires: [web, protocols]\nvars:\n  origin: {required: true}\nprompt: Go from {{origin}}. If nothing, reply NOTHING_TO_REPORT.\n"
	bad := "name: Bad One\nschedule: \"every day\"\nrequires: [teleport]\nprompt: Use api_key=sk-ant-api03-abcdefghijklmnop for {{dest}} now.\n"
	_ = os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(good), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(bad), 0o644)
	ps, files := LintDir(dir, []string{"fetch_url"})
	if files != 2 {
		t.Fatalf("files %d", files)
	}
	var goodProblems, badProblems []string
	for _, p := range ps {
		if p.File == "good.yaml" {
			goodProblems = append(goodProblems, p.String())
		} else {
			badProblems = append(badProblems, p.String())
		}
	}
	if len(goodProblems) != 0 {
		t.Fatalf("good file should be clean: %v", goodProblems)
	}
	joined := strings.Join(badProblems, "\n")
	for _, want := range []string{"schedule is not a valid", "{{dest}}", "an API key", "teleport", "lowercase"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing finding %q in:\n%s", want, joined)
		}
	}
	if !Errors(ps) {
		t.Fatal("expected errors")
	}
}

// Lint used to reject any prompt with "password" or "secret" in it.
func TestLintLooksForCredentialsNotWords(t *testing.T) {
	cases := []struct {
		text string
		want string // "" means clean
	}{
		{"Plan the office Secret Santa draw.", ""},
		{"Remind me to change my password every quarter.", ""},
		{"If the site asks for a password: stop and ask the user.", ""},
		{"Use the token: {{token}} from vars.", ""},
		{"Log in with password: hunter22", "a password or key written out"},
		{"export OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstu", "an API key"},
		{"clone with ghp_abcdefghijklmnopqrstuvwxyz0123", "a GitHub token"},
		{"post as xoxb-1234567890-abcdefghij", "a Slack token"},
		{"aws AKIAABCDEFGHIJKLMNOP", "an AWS access key"},
		{"-----BEGIN OPENSSH PRIVATE KEY-----", "a private key"},
	}
	for _, c := range cases {
		if got := credentialIn(c.text); got != c.want && !(c.want != "" && strings.Contains(got, c.want)) {
			t.Errorf("credentialIn(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// A pack is linted the way it is loaded: protocols/ when it has one (so a
// mkdocs.yml at the root is not a failing "protocol"), plus its personas.
func TestLintPackLayout(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "pack.yaml"), "name: p\ndescription: d\nrepo: https://github.com/you/p\n")
	write(t, filepath.Join(dir, "mkdocs.yml"), "site_name: docs\n")
	write(t, filepath.Join(dir, "protocols", "news.yaml"), "name: news\ndescription: d\nversion: 0.1.0\nauthor: a\nprompt: read the news\n")
	write(t, filepath.Join(dir, "personas", "mirrin.yaml"), "name: Butler\ntagline: t\nversion: 0.1.0\nauthor: a\ncharacter: dry\n")
	write(t, filepath.Join(dir, "personas", "broken.yaml"), "name: Nobody\n")
	ps, files := LintDir(dir, nil)
	if files != 3 {
		t.Fatalf("files %d: %v", files, ps)
	}
	joined := ""
	for _, p := range ps {
		joined += p.String() + "\n"
	}
	if strings.Contains(joined, "mkdocs") {
		t.Errorf("root YAML of a pack with protocols/ was linted:\n%s", joined)
	}
	for _, want := range []string{"personas/broken.yaml: error: name and character are required", `personas/mirrin.yaml: warn: id "mirrin" is a built-in persona's`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

// The pack template had no persona to copy, and lint said nothing about a
// clean one. The template now carries one that passes, and lint names it.
func TestPackTemplateLintsCleanWithItsPersona(t *testing.T) {
	ps, files := LintDir(filepath.Join("..", "..", "examples", "pack-template"), KnownSkills)
	if Errors(ps) {
		t.Fatalf("the pack template fails lint: %v", ps)
	}
	if files != 2 {
		t.Fatalf("linted %d files, want the protocol and the persona: %v", files, ps)
	}
	found := false
	for _, p := range ps {
		if p.Level == "warn" {
			t.Errorf("the template should lint without warnings: %s", p)
		}
		if p.File == "personas/navigator.yaml" && p.Level == "info" && strings.Contains(p.Message, "mirrin-pack-my-pack/navigator") {
			found = true
		}
	}
	if !found {
		t.Fatalf("lint didn't report the persona: %v", ps)
	}
}
