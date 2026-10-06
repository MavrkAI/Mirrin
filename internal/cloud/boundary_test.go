package cloud_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// No crippling, provably: the free twin cannot ask whether the user pays.
// Cloud code lives in internal/cloud, and only the packages that wire the
// twin together may import it. Every other package in the module, which is
// every feature, can neither import it nor reach it through one that does.
const module = "github.com/MavrkAI/Mirrin"

const cloudPkg = module + "/internal/cloud"

// allowedImporters may import internal/cloud, as may internal/cloud's own
// subpackages.
var allowedImporters = []string{
	module + "/cmd/mirrin",
	module + "/internal/daemon",
	module + "/internal/reach",
}

func TestOnlyTheWiringSeesCloud(t *testing.T) {
	root := moduleRoot(t)
	v := graphViolations(goList(t, root, nil))
	v = append(v, sourceViolations(t, root)...)
	for _, s := range v {
		t.Error(s)
	}
	if len(v) > 0 {
		t.Log("Core packages must not see Cloud. Pass what they need in from internal/daemon as a plain value or interface.")
	}
}

// The checks fail when a core package adds the import. The fixture module
// in testdata/boundary does it three ways: directly (internal/api), through
// an allowed package (internal/agent through internal/reach), in a file for
// another platform (internal/channels/chat), and in a test (internal/memory).
func TestBoundaryCheckCatchesTheImport(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "boundary"))
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off", "GOTOOLCHAIN=local"}
	got := append(graphViolations(goList(t, dir, env)), sourceViolations(t, dir)...)
	for _, want := range []string{
		"internal/api imports " + cloudPkg,
		"internal/agent depends on " + cloudPkg + " through " + module + "/internal/reach",
		"internal/channels/chat imports " + cloudPkg + " in chat_plan9.go",
		"internal/memory imports " + cloudPkg + "/cloudtest in its tests",
	} {
		if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, want) }) {
			t.Errorf("not caught: %s", want)
		}
	}
	for _, s := range got {
		for _, ok := range []string{"cmd/mirrin ", "internal/daemon ", "internal/reach ", "internal/cloud/cloudtest "} {
			if strings.HasPrefix(s, ok) {
				t.Errorf("an allowed importer was reported: %s", s)
			}
		}
	}
	if t.Failed() {
		t.Logf("reported:\n%s", strings.Join(got, "\n"))
	}
}

func allowed(pkg string) bool {
	return slices.Contains(allowedImporters, pkg) || isCloud(pkg)
}

func isCloud(pkg string) bool {
	return pkg == cloudPkg || strings.HasPrefix(pkg, cloudPkg+"/")
}

// listed is the part of `go list -json` the check reads.
type listed struct {
	ImportPath   string
	Module       *struct{ Path string }
	Imports      []string
	TestImports  []string
	XTestImports []string
	Deps         []string
}

// goList runs `go list -deps -json ./...` in dir.
func goList(t *testing.T, dir string, env []string) []listed {
	t.Helper()
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	cmd := exec.Command(gotool, "list", "-deps", "-json=ImportPath,Module,Imports,TestImports,XTestImports,Deps", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list in %s: %v\n%s", dir, err, stderr.String())
	}
	var pkgs []listed
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p listed
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list found no packages")
	}
	return pkgs
}

// graphViolations reads the import graph: a package of this module that is
// not allowed may not import internal/cloud (in its code or its tests), nor
// depend on it through anything else.
func graphViolations(pkgs []listed) []string {
	byPath := map[string]listed{}
	for _, p := range pkgs {
		byPath[p.ImportPath] = p
	}
	var out []string
	for _, p := range pkgs {
		if p.Module == nil || p.Module.Path != module || allowed(p.ImportPath) {
			continue
		}
		name := strings.TrimPrefix(p.ImportPath, module+"/")
		direct := false
		for _, imp := range p.Imports {
			if isCloud(imp) {
				out, direct = append(out, fmt.Sprintf("%s imports %s", name, imp)), true
			}
		}
		for _, imp := range slices.Concat(p.TestImports, p.XTestImports) {
			if isCloud(imp) {
				out = append(out, fmt.Sprintf("%s imports %s in its tests", name, imp))
			}
		}
		if direct || !slices.ContainsFunc(p.Deps, isCloud) {
			continue
		}
		for _, imp := range p.Imports {
			if slices.ContainsFunc(byPath[imp].Deps, isCloud) {
				out = append(out, fmt.Sprintf("%s depends on %s through %s", name, cloudPkg, imp))
				break
			}
		}
	}
	return out
}

// sourceViolations reads every .go file's imports, whatever its build
// constraints, since go list sees only this platform's files. Directories
// go ignores (testdata, vendor, dot and underscore) and nested modules are
// skipped.
func sourceViolations(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p != root && (n == "testdata" || n == "vendor" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") || exists(filepath.Join(p, "go.mod"))) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		pkg := module
		if rel != "." {
			pkg += "/" + filepath.ToSlash(rel)
		}
		if allowed(pkg) {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, s := range f.Imports {
			imp, _ := strconv.Unquote(s.Path.Value)
			if isCloud(imp) {
				out = append(out, fmt.Sprintf("%s imports %s in %s", strings.TrimPrefix(pkg, module+"/"), imp, filepath.Base(p)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// moduleRoot is the directory of this module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.HasPrefix(string(b), "module "+module+"\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod for " + module + " above the test")
		}
		dir = parent
	}
}

// The twin never pitches Cloud. The product's name stays out of the page
// the twin shows (ui.html), who it is (personas), what it is told
// (prompts, in the agent, the scheduled jobs, protocols and tool
// descriptions) and where it talks (channels). Cloud appears only where the
// user asks for the capability: the Reach and Backup pages and
// `mirrin cloud`.
var neverMentionCloud = []string{
	"internal/api/ui.html",
	"internal/persona",
	"internal/agent",
	"internal/channels",
	"internal/heartbeat",
	"internal/protocols",
	"internal/skills",
	"internal/tools",
	"internal/tasks",
	"internal/watch",
	"internal/patterns",
	"internal/approvals",
	"examples",
	"registry",
}

// stringsNeverMentionCloud are packages whose Go code may name Cloud in a
// comment (the daemon's hook that starts the refresh on a linked machine)
// but never in a string the twin could say: internal/daemon holds the
// first-week nudges and the notices it sends the owner. Their other files
// are read whole.
var stringsNeverMentionCloud = []string{
	"internal/daemon",
}

// cloudName matches the product's name however it is spaced or cased, and
// its name from before the rename.
var cloudName = regexp.MustCompile(`(?i)(mirrin|antbot)[\s_-]*cloud`) // rename:keep

func TestTheTwinNeverMentionsCloud(t *testing.T) {
	root := moduleRoot(t)
	hits, err := mentions(root, neverMentionCloud)
	if err != nil {
		t.Fatal(err)
	}
	more, err := stringMentions(root, stringsNeverMentionCloud)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range slices.Concat(hits, more) {
		t.Errorf("%s: the twin never pitches Cloud; keep the name to the Reach and Backup pages and `mirrin cloud`", h)
	}
}

// The grep finds the name in the fixture, however it is written, and in the
// daemon's strings but not its comments or tests.
func TestMentionsCheckFindsTheName(t *testing.T) {
	dir := filepath.Join("testdata", "mentions")
	hits, err := mentions(dir, []string{"internal/persona", "internal/channels"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join("internal", "channels", "chat", "chat.go") + ":4",
		filepath.Join("internal", "persona", "pushy.yaml") + ":3",
		filepath.Join("internal", "persona", "pushy.yaml") + ":4",
	}
	if !slices.Equal(hits, want) {
		t.Fatalf("found %q, want %q", hits, want)
	}
	hits, err = stringMentions(dir, []string{"internal/daemon"})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		filepath.Join("internal", "daemon", "daemon.go") + ":10",
		filepath.Join("internal", "daemon", "daemon.go") + ":7",
	}
	if !slices.Equal(hits, want) {
		t.Fatalf("in the daemon's strings, found %q, want %q", hits, want)
	}
}

// stringMentions is mentions for packages where only what the code can say
// counts: in .go files, string literals (tests skipped); other files whole.
func stringMentions(root string, paths []string) ([]string, error) {
	var hits []string
	fset := token.NewFileSet()
	for _, p := range paths {
		err := filepath.WalkDir(filepath.Join(root, p), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if !strings.HasSuffix(path, ".go") {
				h, err := mentions(root, []string{rel})
				hits = append(hits, h...)
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					s = lit.Value
				}
				if cloudName.MatchString(s) {
					hits = append(hits, fmt.Sprintf("%s:%d", rel, fset.Position(lit.Pos()).Line))
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(hits)
	return hits, nil
}

// mentions returns file:line for every match of cloudName in paths under
// root, files or directories, skipping files over 4 MiB (models, audio).
func mentions(root string, paths []string) ([]string, error) {
	var hits []string
	for _, p := range paths {
		err := filepath.WalkDir(filepath.Join(root, p), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if fi, err := d.Info(); err != nil || fi.Size() > 4<<20 {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
			for n := 1; sc.Scan(); n++ {
				if cloudName.Match(sc.Bytes()) {
					rel, _ := filepath.Rel(root, path)
					hits = append(hits, fmt.Sprintf("%s:%d", rel, n))
				}
			}
			return sc.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(hits)
	return hits, nil
}
