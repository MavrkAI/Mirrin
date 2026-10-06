package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// cloudCLI runs `mirrin cloud args…` against f, with the browser replaced by
// browse, and returns what it printed.
func cloudCLI(t *testing.T, cfg *config.Config, input string, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := runCloud(t.Context(), cfg, args, strings.NewReader(input), &out)
	return out.String(), err
}

func fakeCloud(t *testing.T) (*cloudtest.Fake, *config.Config, *[]string) {
	t.Helper()
	t.Setenv("MIRRIN_HOME", t.TempDir())
	f := cloudtest.NewFake(t)
	opened := &[]string{}
	oldOpen, oldEvery, oldKeys := openBrowser, linkPollEvery, cloudKeys
	openBrowser = func(u string) error {
		*opened = append(*opened, u)
		return nil
	}
	linkPollEvery = 10 * time.Millisecond
	cloudKeys = func() map[string]ed25519.PublicKey { return f.Keys() }
	t.Cleanup(func() { openBrowser, linkPollEvery, cloudKeys = oldOpen, oldEvery, oldKeys })
	cfg := config.Default()
	cfg.Cloud.API = f.URL
	return f, cfg, opened
}

// A whole round: status before (nothing), link through a paid checkout,
// status, me, egress, billing, delete and undo, unlink, status after.
func TestCloudCommands(t *testing.T) {
	f, cfg, opened := fakeCloud(t)
	out, err := cloudCLI(t, cfg, "", "status")
	if err != nil || !strings.Contains(out, "Not linked") {
		t.Fatalf("status before linking: %q %v", out, err)
	}
	if out, _ := cloudCLI(t, cfg, "", "egress"); !strings.Contains(out, "Nothing has been sent") {
		t.Fatalf("egress before linking: %q", out)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "cloud")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("status or egress created data/cloud")
	}

	// The browser pays: open the checkout page a moment later.
	openBrowser = func(u string) error {
		*opened = append(*opened, u)
		go func() {
			time.Sleep(50 * time.Millisecond)
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	out, err = cloudCLI(t, cfg, "", "link", "--handle", "quiet-wren-07")
	if err != nil {
		t.Fatalf("link: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Linked: this machine is quiet-wren-07.mirrin.link (active)") || len(*opened) != 1 || !strings.Contains((*opened)[0], "/checkout/") {
		t.Fatalf("link printed %q and opened %v", out, *opened)
	}
	if out, _ := cloudCLI(t, cfg, "", "status"); !strings.Contains(out, "State:          active") || !strings.Contains(out, "https://quiet-wren-07.mirrin.link") {
		t.Fatalf("status: %q", out)
	}
	if out, err := cloudCLI(t, cfg, "", "me"); err != nil || !strings.Contains(out, `"name": "quiet-wren-07"`) {
		t.Fatalf("me: %q %v", out, err)
	}
	if out, err := cloudCLI(t, cfg, "", "billing"); err != nil || !strings.Contains(out, f.URL+"/portal/") {
		t.Fatalf("billing: %q %v", out, err)
	}
	out, _ = cloudCLI(t, cfg, "", "egress")
	for _, want := range []string{"POST   " + strings.TrimPrefix(f.URL, "http://") + "/v1/link/start", "GET    ", "/v1/me", "/v1/billing/portal", "requests;"} {
		if !strings.Contains(out, want) {
			t.Errorf("egress lacks %q:\n%s", want, out)
		}
	}

	if _, err := cloudCLI(t, cfg, "wrong-handle\n", "delete-account"); err == nil {
		t.Fatal("delete-account went ahead without the handle typed")
	}
	if out, err := cloudCLI(t, cfg, "quiet-wren-07\n", "delete-account"); err != nil || !strings.Contains(out, "Scheduled for deletion") {
		t.Fatalf("delete-account: %q %v", out, err)
	}
	if out, _ := cloudCLI(t, cfg, "", "status"); !strings.Contains(out, "Deletion:       scheduled") {
		t.Fatalf("status after delete-account: %q", out)
	}
	if out, err := cloudCLI(t, cfg, "", "delete-account", "--undo"); err != nil || !strings.Contains(out, "cancelled") {
		t.Fatalf("delete-account --undo: %q %v", out, err)
	}

	if _, err := cloudCLI(t, cfg, "n\n", "unlink"); err == nil {
		t.Fatal("unlink went ahead after no")
	}
	if out, err := cloudCLI(t, cfg, "y\n", "unlink"); err != nil || !strings.Contains(out, "Unlinked") {
		t.Fatalf("unlink: %q %v", out, err)
	}
	if out, _ := cloudCLI(t, cfg, "", "status"); !strings.Contains(out, "Not linked") {
		t.Fatalf("status after unlink: %q", out)
	}
	if out, _ := cloudCLI(t, cfg, "", "egress"); !strings.Contains(out, "DELETE /v1/device") && !strings.Contains(out, "DELETE "+strings.TrimPrefix(f.URL, "http://")+"/v1/device") {
		t.Fatalf("the ledger did not survive unlink:\n%s", out)
	}
}

// A checkout left unfinished is resumed, not started again: it may have
// been paid in a tab this terminal never saw.
func TestCloudLinkResumes(t *testing.T) {
	f, cfg, opened := fakeCloud(t)
	ctx, cancel := context.WithCancel(t.Context())
	openBrowser = func(u string) error { // the user closes the terminal
		*opened = append(*opened, u)
		cancel()
		return nil
	}
	if err := runCloud(ctx, cfg, []string{"link"}, strings.NewReader(""), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted link: %v", err)
	}
	if err := f.Pay(path.Base((*opened)[0])); err != nil { // and pays in the browser anyway
		t.Fatal(err)
	}
	openBrowser = func(u string) error {
		t.Errorf("a second checkout opened: %s", u)
		return nil
	}
	out, err := cloudCLI(t, cfg, "", "link")
	if err != nil || !strings.Contains(out, "Resuming the checkout") || !strings.Contains(out, "Linked:") {
		t.Fatalf("second link: %q %v", out, err)
	}
	starts := 0
	for _, r := range f.Requests() {
		if r.Path == "/v1/link/start" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("%d checkouts started, want 1", starts)
	}
}

// A checkout that expired unpaid is replaced by a new one.
func TestCloudLinkRestartsAnExpiredCheckout(t *testing.T) {
	f, cfg, opened := fakeCloud(t)
	ctx, cancel := context.WithCancel(t.Context())
	openBrowser = func(u string) error {
		*opened = append(*opened, u)
		cancel()
		return nil
	}
	runCloud(ctx, cfg, []string{"link"}, strings.NewReader(""), io.Discard)
	if err := f.ExpireLink(path.Base((*opened)[0])); err != nil {
		t.Fatal(err)
	}
	openBrowser = func(u string) error {
		*opened = append(*opened, u)
		return f.Pay(path.Base(u))
	}
	out, err := cloudCLI(t, cfg, "", "link")
	if err != nil || !strings.Contains(out, "expired unpaid; starting a new one") || !strings.Contains(out, "Linked:") || len(*opened) != 2 {
		t.Fatalf("link after expiry: %q %v, opened %v", out, err, *opened)
	}
}

func TestCloudLinkNeedsTrustedKeys(t *testing.T) {
	_, cfg, opened := fakeCloud(t)
	cloudKeys = func() map[string]ed25519.PublicKey { return nil }
	if _, err := cloudCLI(t, cfg, "", "link"); err == nil || !strings.Contains(err.Error(), "trusts no Mirrin Cloud signing key") {
		t.Fatalf("link in a build with no keys: %v", err)
	}
	if len(*opened) != 0 {
		t.Fatal("a checkout opened in a build that could not verify it")
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "cloud")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("link created data/cloud before refusing")
	}
}

// What the control plane says goes to the terminal with every rune that is
// not graphic escaped, so an answer cannot move the cursor, clear the
// screen or reorder the text.
func TestMeOutputIsGraphicOnly(t *testing.T) {
	in := "{\n  \"email\": \"a\u009b2J\u202eb\u2028c\U000e0001d\u00e9\U0001f600\"\n}"
	want := "{\n  \"email\": \"a\\u009b2J\\u202eb\\u2028c\\udb40\\udc01d\u00e9\U0001f600\"\n}"
	got := graphicOnly([]byte(in))
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	var a, b any
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(got), &b); err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("escaping changed the document: %v", err)
	}
}
