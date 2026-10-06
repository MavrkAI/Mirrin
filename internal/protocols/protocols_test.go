package protocols

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExamplesLoad(t *testing.T) {
	dir := t.TempDir()
	if err := WriteExamples(dir); err != nil {
		t.Fatal(err)
	}
	ps, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != len(Examples) {
		t.Fatalf("loaded %d, want %d", len(ps), len(Examples))
	}
	if _, ok := Find(ps, "Morning Briefing"); !ok {
		t.Fatal("case-insensitive find failed")
	}
}

// examples/protocols is what people browsing the repository see; it had
// drifted from the starters Mirrin writes on first run.
func TestRepoExamplesMatchStarters(t *testing.T) {
	repo := filepath.Join("..", "..", "examples", "protocols")
	entries, err := os.ReadDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(Examples) {
		t.Errorf("%s has %d files, the starters %d", repo, len(entries), len(Examples))
	}
	for name, body := range Examples {
		if b, err := os.ReadFile(filepath.Join(repo, name)); err != nil || string(b) != body {
			t.Errorf("%s differs from Examples[%q]; copy it from protocols.go", filepath.Join(repo, name), name)
		}
	}
}

// The starters that wait on someone else's reply promise to check for it
// with follow_up, which looks again, rather than a reminder, which only
// reminds.
func TestStartersThatWaitOnAReplyFollowUp(t *testing.T) {
	for _, name := range []string{"chase-refund.yaml", "rebook.yaml"} {
		body := Examples[name]
		if !strings.Contains(body, "set a follow_up for") || !strings.Contains(body, "check for their reply") || strings.Contains(body, "set a reminder") {
			t.Errorf("%s doesn't follow up:\n%s", name, body)
		}
	}
}

func TestPacksVarsAndRequires(t *testing.T) {
	dir := t.TempDir()
	// a local pack with a variable and a requirement
	pack := filepath.Join(dir, "packs", "community", "protocols")
	if err := os.MkdirAll(pack, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack, "news.yaml"), []byte("name: local news\nrequires: [web, calendar]\nvars:\n  source: {default: https://example.com}\nprompt: Read {{source}} for {{city}}.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vars.yaml"), []byte("local news:\n  city: Melbourne\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := Find(ps, "local news")
	if !ok {
		t.Fatalf("pack protocol not loaded: %+v", ps)
	}
	if p.Prompt != "Read https://example.com for Melbourne." || p.Pack != "community" {
		t.Fatalf("vars not applied: %q pack=%q", p.Prompt, p.Pack)
	}
	if m := p.Missing([]string{"fetch_url"}); len(m) != 1 || m[0] != "calendar" {
		t.Fatalf("missing should be [calendar], got %v", m)
	}
	// a local protocol with the same name shadows the pack, without a complaint
	if err := os.WriteFile(filepath.Join(dir, "news.yaml"), []byte("name: local news\nprompt: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ps, skipped := LoadAll(dir)
	p, _ = Find(ps, "local news")
	if p.Prompt != "mine" || p.Pack != "" || len(skipped) != 0 {
		t.Fatalf("local should shadow pack quietly: %+v %v", p, skipped)
	}
	if _, err := Scaffold(dir, "my thing"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "my-thing.yaml")); err != nil {
		t.Fatal("scaffold not written")
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// One bad file, or an unrelated YAML file at a pack's root, used to stop
// every protocol loading, the user's own included.
func TestOneBadFileDoesNotStopTheRest(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "mine.yaml"), "name: mine\nprompt: do my thing\n")
	write(t, filepath.Join(dir, "noprompt.yaml"), "name: half done\n")
	write(t, filepath.Join(dir, "typo.yaml"), "name: [oops\nprompt: x\n")
	write(t, filepath.Join(dir, "twin.yaml"), "name: mine\nprompt: a second mine\n")
	// a pack with the protocols/ layout: its root YAML is not protocols
	write(t, filepath.Join(dir, "packs", "somepack", "mkdocs.yml"), "site_name: docs\n")
	write(t, filepath.Join(dir, "packs", "somepack", "protocols", "news.yaml"), "name: news\nprompt: read the news\n")
	// an older pack with protocols at its root still loads, minus its bad file
	write(t, filepath.Join(dir, "packs", "oldpack", "weather.yaml"), "name: weather\nprompt: check the sky\n")
	write(t, filepath.Join(dir, "packs", "oldpack", "notes.yml"), "title: not a protocol\n")
	write(t, filepath.Join(dir, "packs", "oldpack", "pack.yaml"), "name: oldpack\n")
	// half-installed packs are hidden
	write(t, filepath.Join(dir, "packs", ".next.installing", "protocols", "ghost.yaml"), "name: ghost\nprompt: boo\n")

	ps, err := Load(dir)
	if err != nil {
		t.Fatalf("Load should skip bad files, got %v", err)
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	if want := []string{"mine", "news", "weather"}; !slices.Equal(names, want) {
		t.Fatalf("loaded %v, want %v", names, want)
	}
	_, skipped := LoadAll(dir)
	var got []string
	for _, s := range skipped {
		got = append(got, filepath.Base(s.File))
	}
	slices.Sort(got)
	if want := []string{"noprompt.yaml", "notes.yml", "twin.yaml", "typo.yaml"}; !slices.Equal(got, want) {
		t.Fatalf("skipped %v, want %v", got, want)
	}
}

// Write and Scaffold used to overwrite existing files, and every name in a
// non-Latin script became ".yaml".
func TestWriteNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := WriteExamples(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "morning-briefing.yaml")
	before, _ := os.ReadFile(path)
	if _, err := Scaffold(dir, "Morning Briefing"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("scaffold over an existing protocol should refuse, got %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("morning-briefing.yaml was overwritten")
	}

	a, errA := Write(dir, Protocol{Name: "朝のまとめ", Prompt: "morning"})
	b, errB := Write(dir, Protocol{Name: "夜のまとめ", Prompt: "evening"})
	if errA != nil || errB != nil || a == b || filepath.Base(a) != "朝のまとめ.yaml" {
		t.Fatalf("non-Latin names: %q %v, %q %v", a, errA, b, errB)
	}
	ps, _ := Load(dir)
	if _, ok := Find(ps, "朝のまとめ"); !ok {
		t.Fatal("non-Latin protocol did not load")
	}

	cases := []struct {
		name string
		p    Protocol
		want string
	}{
		{"no letters", Protocol{Name: "!!!", Prompt: "x"}, "letter or number"},
		{"six-field cron", Protocol{Name: "friday wrap", Prompt: "x", Schedule: "0 0 17 * * 5"}, "it has 6, so drop the seconds"},
		{"words for a schedule", Protocol{Name: "daily", Prompt: "x", Schedule: "every day"}, "5 fields"},
		{"no prompt", Protocol{Name: "empty"}, "name and a prompt"},
	}
	for _, c := range cases {
		path, err := Write(dir, c.p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error about %q", c.name, err, c.want)
		}
		if path != "" {
			t.Errorf("%s: wrote %s", c.name, path)
		}
		if err != nil && strings.Contains(err.Error(), "expected") {
			t.Errorf("%s: cron's own message leaked: %v", c.name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "friday-wrap.yaml")); err == nil {
		t.Fatal("a protocol with a bad schedule was saved")
	}
}

// requires: [email] used to accept only the IMAP tools, and [protocols]
// passed lint but was always reported missing.
func TestMissingAcceptsAnyToolForASkill(t *testing.T) {
	cases := []struct {
		requires []string
		have     []string
		want     []string
	}{
		{[]string{"email"}, []string{"gmail_search", "gmail_read"}, nil},
		{[]string{"email"}, []string{"list_emails"}, nil},
		{[]string{"email"}, []string{"fetch_url"}, []string{"email"}},
		{[]string{"protocols"}, []string{"list_protocols", "run_protocol"}, nil},
		{[]string{"browser"}, []string{"browser_inspect"}, nil},
		{[]string{"gmail_send"}, []string{"gmail_send"}, nil},
		{[]string{"calendar", "web"}, []string{"fetch_url"}, []string{"calendar"}},
	}
	for _, c := range cases {
		got := Protocol{Requires: c.requires}.Missing(c.have)
		if !slices.Equal(got, c.want) {
			t.Errorf("requires %v with %v: missing %v, want %v", c.requires, c.have, got, c.want)
		}
	}
	// every skill lint accepts is one Missing can satisfy
	for _, s := range KnownSkills {
		if len(skillTools[s]) == 0 {
			t.Errorf("skill %q has no tools", s)
		}
	}
}

// `protocols check` re-rendered prompts with no values, so vars set in
// vars.yaml were reported as missing.
func TestUnsetVarsAfterLoad(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "commute.yaml"), "name: commute check\nvars:\n  origin: {required: true}\n  dest: {default: work}\nprompt: From {{origin}} to {{dest}}.\n")
	ps, _ := Load(dir)
	if p, _ := Find(ps, "commute check"); !slices.Equal(p.Unset, []string{"origin"}) {
		t.Fatalf("before vars.yaml: unset %v", p.Unset)
	}
	write(t, VarsPath(dir), "commute check:\n  origin: home\n")
	ps, _ = Load(dir)
	if p, _ := Find(ps, "commute check"); len(p.Unset) != 0 || p.Prompt != "From home to work." {
		t.Fatalf("after vars.yaml: unset %v prompt %q", p.Unset, p.Prompt)
	}
}

// The starters work without a messaging app: a briefing for someone who
// only has the screen mustn't be written as a WhatsApp message.
func TestExamplesNameNoMessagingApp(t *testing.T) {
	for name, body := range Examples {
		for _, app := range []string{"whatsapp", "telegram"} {
			if strings.Contains(strings.ToLower(body), app) {
				t.Errorf("%s says %s", name, app)
			}
		}
	}
}
