package api

import (
	"os"
	"strings"
	"testing"
)

// The Trust page and docs/cloud-trust.md show the same list of what a
// hosted relay's operator can and can't see, word for word.
func TestVisibilityMatchesTheDocs(t *testing.T) {
	doc, err := os.ReadFile("../../docs/cloud-trust.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(trustVisibility) == "" || !strings.Contains(string(doc), trustVisibility) {
		t.Fatal("docs/cloud-trust.md no longer contains internal/api/trust_visibility.md verbatim; copy one into the other")
	}
}

func TestVisibilityOnTheTrustPage(t *testing.T) {
	e, _ := pagesEnv(t)
	w := e.do(onLoopback, req{path: "/trust", header: map[string]string{"Accept": "text/html", "Authorization": "Bearer " + master}})
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, visibilityMark) {
		t.Fatalf("%d, list not in place", w.Code)
	}
	for _, want := range []string{"<h3>What we can see</h3>", "<h3>What we can&#39;t see</h3>", `<div class="vrow"><strong>Your 12 words</strong>`, "<code>mirrin reach verify</code>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Trust page lacks %s", want)
		}
	}
	if strings.Contains(body, "|---|") {
		t.Error("raw markdown on the page")
	}
}

func TestVisibilityHTMLEscapes(t *testing.T) {
	got := visibilityHTML("## A <b>\n\n| What | Why |\n|---|---|\n| `x<y>` | a & b |\n")
	want := `<h3>A &lt;b&gt;</h3><div class="vrow"><strong><code>x&lt;y&gt;</code></strong><dl><dt>Why</dt><dd>a &amp; b</dd></dl></div>`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
