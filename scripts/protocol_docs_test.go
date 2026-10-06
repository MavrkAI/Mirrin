package scripts

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The protocol docs (docs/protocols/*.md) quote the CLI, lint and the twin
// word for word, and link to each other. These tests hold them to the code:
// a subcommand the CLI doesn't have, a message reworded in the code, or a
// link to a page or heading that moved fails here rather than in a reader's
// terminal.

// The pages written alongside these, on their own branch (packs, the
// registry, security, contributing). The two land together. While none of
// them is in the tree, a link to one is logged rather than failed; once any
// of them is here, they all must be, and a link to a missing one fails like
// any other.
var laterProtocolPages = map[string]bool{"packs.md": true, "registry.md": true, "security.md": true, "contributing.md": true}

// laterPagesPending says whether none of laterProtocolPages is in dir yet.
func laterPagesPending(dir string) bool {
	for name := range laterProtocolPages {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return false
		}
	}
	return true
}

// protocolPages reads every page in docs/protocols, by file name.
func protocolPages(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoFile("docs/protocols"), "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		pages[filepath.Base(p)] = string(b)
	}
	for _, want := range []string{"README.md", "quickstart.md", "reference.md", "writing-guide.md"} {
		if _, ok := pages[want]; !ok {
			t.Fatalf("docs/protocols/%s is missing", want)
		}
	}
	return pages
}

var reCodeSpan = regexp.MustCompile("`([^`]+)`")

// codeIn is the code on a markdown page: each line of a fenced block, and
// each inline code span.
func codeIn(md string) []string {
	var out []string
	fenced := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			out = append(out, line)
			continue
		}
		for _, m := range reCodeSpan.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// proseOf is a markdown page without its code, so a link shown as an
// example isn't taken for a link.
func proseOf(md string) string {
	var b strings.Builder
	fenced := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if !fenced {
			b.WriteString(reCodeSpan.ReplaceAllString(line, "") + "\n")
		}
	}
	return b.String()
}

// goFiles parses the non-test Go files in a folder of the repo.
func goFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoFile(dir), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatalf("no Go files in %s; update this test to find where the code went", dir)
	}
	return out
}

// Every `mirrin protocols <sub>` the protocol docs show is a subcommand
// protocolsCmd (cmd/mirrin/main.go) has, with flags it takes; and the
// reference shows every one of them.
func TestProtocolDocsNameRealSubcommands(t *testing.T) {
	fset := token.NewFileSet()
	files := goFiles(t, fset, "cmd/mirrin")
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	cmd := funcs["protocolsCmd"]
	if cmd == nil {
		t.Fatal("cmd/mirrin has no protocolsCmd; update this test to find where `mirrin protocols` is handled")
	}
	// The subcommands are the cases of its switch ("list" is also the
	// default); the flags are the literals that look like flags in it and in
	// the functions it calls (updatePacks's --yes).
	subs, flags := map[string]bool{"list": true}, map[string]bool{}
	reFlag := regexp.MustCompile(`^--?[a-z][a-z-]*$`)
	flagsIn := func(fn *ast.FuncDecl) {
		ast.Inspect(fn, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && reFlag.MatchString(s) {
					flags[s] = true
				}
			}
			return true
		})
	}
	flagsIn(cmd)
	ast.Inspect(cmd, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CaseClause:
			for _, e := range x.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						subs[s] = true
					}
				}
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && funcs[id.Name] != nil && id.Name != cmd.Name.Name {
				flagsIn(funcs[id.Name])
			}
		}
		return true
	})
	if len(subs) < 5 {
		t.Fatalf("only %d subcommands found in protocolsCmd; the switch changed shape", len(subs))
	}

	const call = "mirrin protocols"
	reWord := regexp.MustCompile(`^[a-z][a-z-]*$`)
	seen, shown := 0, map[string]bool{}
	for name, md := range protocolPages(t) {
		for _, code := range codeIn(md) {
			for rest := code; ; {
				i := strings.Index(rest, call)
				if i < 0 {
					break
				}
				rest = rest[i+len(call):]
				args := rest
				if j := strings.Index(args, call); j >= 0 {
					args = args[:j] // two commands in one span
				}
				if j := strings.Index(args, " #"); j >= 0 {
					args = args[:j] // a shell comment
				}
				words := strings.Fields(args)
				seen++
				if len(words) == 0 || !reWord.MatchString(words[0]) {
					continue // `mirrin protocols` alone lists; `<cmd>` is a placeholder
				}
				if !subs[words[0]] {
					t.Errorf("docs/protocols/%s shows `mirrin protocols %s`, which protocolsCmd doesn't have", name, words[0])
				}
				if name == "reference.md" {
					shown[words[0]] = true
				}
				for _, w := range words[1:] {
					w = strings.Trim(w, "[](),.;:")
					if strings.HasPrefix(w, "-") && !flags[w] {
						t.Errorf("docs/protocols/%s shows `mirrin protocols %s … %s`, a flag cmd/mirrin doesn't take", name, words[0], w)
					}
				}
			}
		}
	}
	if seen < 10 {
		t.Fatalf("only %d `mirrin protocols` commands found in docs/protocols; the pattern no longer matches how they're written", seen)
	}
	for s := range subs {
		if !shown[s] {
			t.Errorf("docs/protocols/reference.md doesn't show `mirrin protocols %s`; every subcommand belongs in the reference", s)
		}
	}
}

// msgPattern is a message the code can produce: its literal text, with
// each fmt verb, and anything computed, standing for any text.
type msgPattern struct {
	re     *regexp.Regexp
	chunks []string // the literal text, to rule out most patterns before the regexp
	show   string   // the message with … for each gap
	level  string   // lint's level, for lint's findings
}

func (p *msgPattern) matches(msg string) bool {
	for _, c := range p.chunks {
		if !strings.Contains(msg, c) {
			return false
		}
	}
	return p.re.MatchString(msg)
}

// gap stands for text the code fills in.
const gap = "\x00"

var reVerb = regexp.MustCompile(`%(?:\[\d+\])?[-+# 0]*(?:\d+|\*)?(?:\.(?:\d+|\*))?[a-zA-Z%]`)

// flatten is the text an expression makes, with a gap for each fmt verb and
// for anything that isn't a string literal: a concatenation is followed
// through, fmt.Sprintf and fmt.Errorf give their format, errors.New its
// text, and x.Error() the text of a package-level error x (errs).
func flatten(e ast.Expr, errs map[string]string) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			break
		}
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			break
		}
		return reVerb.ReplaceAllStringFunc(s, func(v string) string {
			if v == "%%" {
				return "%"
			}
			return gap
		})
	case *ast.ParenExpr:
		return flatten(x.X, errs)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return flatten(x.X, errs) + flatten(x.Y, errs)
		}
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			break
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			break
		}
		switch {
		case id.Name == "fmt" && (sel.Sel.Name == "Sprintf" || sel.Sel.Name == "Errorf") && len(x.Args) > 0:
			return flatten(x.Args[0], errs)
		case id.Name == "errors" && sel.Sel.Name == "New" && len(x.Args) == 1:
			return flatten(x.Args[0], errs)
		case sel.Sel.Name == "Error" && len(x.Args) == 0 && errs[id.Name] != "":
			return errs[id.Name]
		}
	}
	return gap
}

// newPattern makes a pattern of flattened text, or nil when it has too
// little literal text to say anything.
func newPattern(flat, level string) *msgPattern {
	flat = strings.TrimSpace(flat)
	if !utf8.ValidString(flat) {
		return nil // bytes, not a message
	}
	parts := strings.Split(flat, gap)
	var chunks, quoted []string
	n := 0
	for _, p := range parts {
		quoted = append(quoted, regexp.QuoteMeta(p))
		if p != "" {
			chunks = append(chunks, p)
			n += len(p)
		}
	}
	if n < 6 {
		return nil
	}
	return &msgPattern{
		re:     regexp.MustCompile(`(?s)^\s*` + strings.Join(quoted, `.*?`) + `\s*$`),
		chunks: chunks,
		show:   strings.Join(parts, "…"),
		level:  level,
	}
}

// errorVars maps each package-level `x = errors.New("…")` to its text.
func errorVars(files []*ast.File, errs map[string]string) {
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if text := flatten(vs.Values[i], nil); text != gap && !strings.Contains(text, gap) {
							errs[name.Name] = text
						}
					}
				}
			}
		}
	}
}

// lintFindings are the findings internal/protocols/lint.go can report:
// each add(level, message) and Problem{file, level, message}.
func lintFindings(t *testing.T) []*msgPattern {
	t.Helper()
	fset := token.NewFileSet()
	errs := map[string]string{}
	errorVars(goFiles(t, fset, "internal/protocols"), errs)
	f, err := parser.ParseFile(fset, repoFile("internal/protocols/lint.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []*msgPattern
	record := func(level, msg ast.Expr) {
		lit, ok := level.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		lv, _ := strconv.Unquote(lit.Value)
		if p := newPattern(flatten(msg, errs), lv); p != nil {
			out = append(out, p)
		}
	}
	problem := func(c *ast.CompositeLit) {
		if len(c.Elts) == 3 {
			record(c.Elts[1], c.Elts[2])
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "add" && len(x.Args) == 2 {
				record(x.Args[0], x.Args[1])
			}
		case *ast.CompositeLit:
			switch ty := x.Type.(type) {
			case *ast.Ident:
				if ty.Name == "Problem" {
					problem(x)
				}
			case *ast.ArrayType: // []Problem{{file, level, message}}
				if id, ok := ty.Elt.(*ast.Ident); ok && id.Name == "Problem" {
					for _, e := range x.Elts {
						if c, ok := e.(*ast.CompositeLit); ok {
							problem(c)
						}
					}
				}
			}
		}
		return true
	})
	if len(out) < 15 {
		t.Fatalf("only %d findings read from lint.go; it changed shape, so update lintFindings", len(out))
	}
	return out
}

// splitRow splits a markdown table row on the pipes that aren't escaped.
func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cur.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// quotedRow is one table row's messages, with its other cells by header.
type quotedRow struct {
	cells    map[string]string
	messages []string
}

// quotedRows reads the tables in md that have a Message column: each row's
// code spans in that column, and its cells by header (lowercase).
func quotedRows(md string) []quotedRow {
	var out []quotedRow
	var header []string
	for _, line := range append(strings.Split(md, "\n"), "") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			header = nil
			continue
		}
		cells := splitRow(line)
		if header == nil {
			header = cells
			continue
		}
		if strings.Trim(strings.Join(cells, ""), "-: ") == "" {
			continue // the rule under the header
		}
		row := quotedRow{cells: map[string]string{}}
		for i, c := range cells {
			if i >= len(header) {
				break
			}
			h := strings.ToLower(header[i])
			row.cells[h] = c
			if h == "message" {
				for _, m := range reCodeSpan.FindAllStringSubmatch(c, -1) {
					row.messages = append(row.messages, strings.TrimSpace(m[1]))
				}
			}
		}
		if _, ok := row.cells["message"]; ok {
			out = append(out, row)
		}
	}
	return out
}

// markedBlock is the text after start and before the next end.
func markedBlock(t *testing.T, s, where, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("%s has no %s marker", where, start)
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		t.Fatalf("%s has %s but no %s", where, start, end)
	}
	return s[:j]
}

// Every message in the reference's lint table is one lint.go reports, at
// the level the table gives; every finding lint.go has is in the table; and
// every finding shown in an example output on any page is one lint can give.
func TestProtocolDocsQuoteLintExactly(t *testing.T) {
	findings := lintFindings(t)
	pages := protocolPages(t)
	table := markedBlock(t, pages["reference.md"], "docs/protocols/reference.md", "<!-- lint-rules:start -->", "<!-- lint-rules:end -->")
	rows := quotedRows(table)
	if len(rows) < 15 {
		t.Fatalf("only %d rows read from the lint table in docs/protocols/reference.md; it should list every rule", len(rows))
	}
	listed := make([]bool, len(findings))
	for _, r := range rows {
		if len(r.messages) == 0 {
			t.Errorf("docs/protocols/reference.md: a lint table row quotes no message: %q", r.cells["message"])
		}
		for _, msg := range r.messages {
			level, found, other := r.cells["level"], false, ""
			for i, f := range findings {
				if !f.matches(msg) {
					continue
				}
				if f.level == level {
					found, listed[i] = true, true
				} else {
					other = f.level
				}
			}
			switch {
			case found:
			case other != "":
				t.Errorf("docs/protocols/reference.md lists %q as %s, but lint.go reports it as %s", msg, level, other)
			default:
				t.Errorf("docs/protocols/reference.md quotes the lint message %q, which internal/protocols/lint.go doesn't have", msg)
			}
		}
	}
	for i, f := range findings {
		if !listed[i] {
			t.Errorf("internal/protocols/lint.go reports %q (%s), which the lint table in docs/protocols/reference.md doesn't list", f.show, f.level)
		}
	}

	reShown := regexp.MustCompile(`^\s*\S+: (error|warn|info): (.+)$`)
	shown := 0
	for name, md := range pages {
		for _, line := range codeIn(md) {
			m := reShown.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			shown++
			ok := false
			for _, f := range findings {
				ok = ok || (f.level == m[1] && f.matches(m[2]))
			}
			if !ok {
				t.Errorf("docs/protocols/%s shows lint saying %q, which internal/protocols/lint.go can't (at %s)", name, m[2], m[1])
			}
		}
	}
	if shown < 3 {
		t.Fatalf("only %d lint findings found in example output in docs/protocols; the pattern no longer matches how they're shown", shown)
	}
}

// Every other message the reference quotes (in a table with a Message
// column between its messages markers) is one the code can produce.
func TestProtocolDocsQuoteMessagesExactly(t *testing.T) {
	fset := token.NewFileSet()
	errs := map[string]string{}
	var files []*ast.File
	for _, dir := range []string{"internal/protocols", "internal/heartbeat", "internal/skills/protocols", "internal/daemon", "internal/agent", "cmd/mirrin"} {
		fs := goFiles(t, fset, dir)
		errorVars(fs, errs)
		files = append(files, fs...)
	}
	var known []*msgPattern
	add := func(flat string) {
		if p := newPattern(flat, ""); p != nil {
			known = append(known, p)
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op == token.ADD {
					add(flatten(x, errs))
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					flat := flatten(x, errs)
					add(flat)
					if strings.Contains(flat, "\n") { // one line of a longer message
						for _, line := range strings.Split(flat, "\n") {
							add(line)
						}
					}
				}
			case *ast.CallExpr:
				if s := flatten(x, errs); s != gap {
					add(s) // errors.New, fmt.Errorf, or a known error's text
				}
			}
			return true
		})
	}

	ref := protocolPages(t)["reference.md"]
	const start, end = "<!-- messages:start -->", "<!-- messages:end -->"
	quoted := 0
	for rest := ref; strings.Contains(rest, start); {
		block := markedBlock(t, rest, "docs/protocols/reference.md", start, end)
		rest = rest[strings.Index(rest, start)+len(start):]
		rest = rest[strings.Index(rest, end)+len(end):]
		for _, r := range quotedRows(block) {
			if len(r.messages) == 0 {
				t.Errorf("docs/protocols/reference.md: a message table row quotes no message: %q", r.cells["message"])
			}
			for _, msg := range r.messages {
				quoted++
				ok := false
				for _, p := range known {
					if p.matches(msg) {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("docs/protocols/reference.md quotes %q, which the code doesn't say (internal/protocols, heartbeat, skills/protocols, daemon, agent or cmd/mirrin)", msg)
				}
			}
		}
	}
	if quoted < 20 {
		t.Fatalf("only %d messages read from the message tables in docs/protocols/reference.md; they should sit between %s and %s", quoted, start, end)
	}
}

var reLink = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// anchors are the heading anchors GitHub makes for a markdown page.
func anchors(md string) map[string]bool {
	out := map[string]bool{}
	fenced := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimSpace(strings.TrimRight(strings.TrimLeft(line, "#"), "# "))
		var b strings.Builder
		for _, r := range strings.ToLower(text) {
			switch {
			case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
				b.WriteRune(r)
			case r == ' ':
				b.WriteRune('-')
			}
		}
		slug := b.String()
		for n, s := 1, slug; ; n++ {
			if !out[s] {
				out[s] = true
				break
			}
			s = slug + "-" + strconv.Itoa(n)
		}
	}
	return out
}

// Every relative link on the protocol pages goes to a file that is there,
// and to a heading that is there. Links to the pages written alongside
// these (packs, registry, security, contributing) wait until they arrive,
// and only while none of them has.
func TestProtocolDocsLinksResolve(t *testing.T) {
	dir := repoFile("docs/protocols")
	pending := laterPagesPending(dir)
	reScheme := regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	links := 0
	for name, md := range protocolPages(t) {
		for _, m := range reLink.FindAllStringSubmatch(proseOf(md), -1) {
			target := m[1]
			if reScheme.MatchString(target) {
				continue // a web address
			}
			links++
			path, anchor, _ := strings.Cut(target, "#")
			file := filepath.Join(dir, name)
			if path != "" {
				file = filepath.Join(dir, filepath.FromSlash(path))
			}
			b, err := os.ReadFile(file)
			if err != nil {
				if st, serr := os.Stat(file); serr == nil && st.IsDir() && anchor == "" {
					continue // a folder, such as examples/protocols
				}
				if pending && filepath.Dir(file) == dir && laterProtocolPages[filepath.Base(file)] {
					t.Logf("docs/protocols/%s links to %s, which arrives with the packs, registry, security and contributing pages", name, target)
					continue
				}
				t.Errorf("docs/protocols/%s links to %s, which isn't there", name, target)
				continue
			}
			if anchor != "" && strings.HasSuffix(file, ".md") && !anchors(string(b))[anchor] {
				t.Errorf("docs/protocols/%s links to %s, but %s has no heading #%s", name, target, filepath.Base(file), anchor)
			}
		}
	}
	if links < 10 {
		t.Fatalf("only %d relative links found in docs/protocols; the pattern no longer matches how they're written", links)
	}
}
