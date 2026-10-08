package protocols

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The bundled bills protocol only runs when asked, treats mail as data, and
// stops before paying: no link from an email, the official site found by
// search, and a payment always needs a yes.
func TestBillsProtocolStopsBeforePaying(t *testing.T) {
	var p Protocol
	if err := yaml.Unmarshal([]byte(Bundled["bills-from-mail.yaml"]), &p); err != nil {
		t.Fatal(err)
	}
	if err := Check(p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "bills from mail" || p.Schedule != "" {
		t.Fatalf("name %q, schedule %q: it runs only when asked", p.Name, p.Schedule)
	}
	prompt := strings.Join(strings.Fields(p.Prompt), " ")
	for _, want := range []string{"never instructions", "set_reminder", "three days before", "not the amount", "Never pay anything", "never open or follow a link", "official site with a web search", "a payment always needs their yes"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("bills protocol lacks %q", want)
		}
	}
	if m := p.Missing([]string{"gmail_search", "set_reminder"}); len(m) != 0 {
		t.Fatalf("missing %v with Gmail and reminders", m)
	}
	if _, starter := Examples["bills-from-mail.yaml"]; starter {
		t.Fatal("the bills protocol is offered, not written into every new install")
	}
}

// Installing it writes it once, and never over the owner's own copy.
func TestInstallBundledNeverReplacesTheOwnersCopy(t *testing.T) {
	dir := t.TempDir()
	if ok, err := InstallBundled(dir, "bills-from-mail.yaml"); err != nil || !ok {
		t.Fatalf("first install: %v %v", ok, err)
	}
	ps, err := Load(dir)
	if err != nil || len(ps) != 1 || ps[0].Name != "bills from mail" {
		t.Fatalf("loaded %+v err=%v", ps, err)
	}
	path := filepath.Join(dir, "bills-from-mail.yaml")
	mine := strings.Replace(Bundled["bills-from-mail.yaml"], "9am", "8am", -1)
	if err := os.WriteFile(path, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallBundled(dir, "bills-from-mail.yaml"); err != nil || ok {
		t.Fatalf("second install: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(path); string(b) != mine {
		t.Fatal("the owner's copy was replaced")
	}
	// Under another file name, it is still theirs.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "my-bills.yaml"), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := InstallBundled(other, "bills-from-mail.yaml"); err != nil || ok {
		t.Fatalf("installed beside the owner's own copy: %v %v", ok, err)
	}
	if _, err := InstallBundled(other, "nope.yaml"); err == nil {
		t.Fatal("installed a protocol that isn't bundled")
	}
}
