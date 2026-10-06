package api

import (
	"bytes"
	_ "embed"
	"html"
	"strings"
)

// trustVisibility is what the operator of a hosted relay and backup store
// can and can't see. docs/cloud-trust.md holds the same text, word for word
// (a test checks), so the page and the docs can't disagree.
//
//go:embed trust_visibility.md
var trustVisibility string

// visibilityMark is where the Trust page takes the list.
const visibilityMark = "<!--visibility-->"

// trustPage is trust.html with the visibility list in place.
func trustPage() []byte {
	return bytes.Replace(trustHTML, []byte(visibilityMark), []byte(visibilityHTML(trustVisibility)), 1)
}

// visibilityHTML renders the list's small markdown: "## " headings, and
// tables, shown as one card per row so they fit a phone. `code` spans show
// as code; everything else is escaped.
func visibilityHTML(md string) string {
	var b strings.Builder
	var head []string
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "## "):
			head = nil
			b.WriteString("<h3>" + inlineMD(strings.TrimPrefix(line, "## ")) + "</h3>")
		case strings.HasPrefix(line, "|"):
			cells := strings.Split(strings.Trim(line, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			switch {
			case head == nil:
				head = cells
			case strings.Trim(strings.Join(cells, ""), "-: ") == "":
				// the header's underline
			default:
				b.WriteString(`<div class="vrow"><strong>` + inlineMD(cells[0]) + "</strong><dl>")
				for i := 1; i < len(cells) && i < len(head); i++ {
					b.WriteString("<dt>" + inlineMD(head[i]) + "</dt><dd>" + inlineMD(cells[i]) + "</dd>")
				}
				b.WriteString("</dl></div>")
			}
		case line != "":
			b.WriteString("<p>" + inlineMD(line) + "</p>")
		}
	}
	return b.String()
}

// inlineMD escapes text and shows `spans` as code.
func inlineMD(s string) string {
	parts := strings.Split(s, "`")
	var b strings.Builder
	for i, p := range parts {
		p = html.EscapeString(p)
		if i%2 == 1 && i < len(parts)-1 {
			b.WriteString("<code>" + p + "</code>")
			continue
		}
		if i%2 == 1 {
			b.WriteString("`")
		}
		b.WriteString(p)
	}
	return b.String()
}
