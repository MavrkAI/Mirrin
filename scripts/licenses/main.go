// Command licenses finds every third-party module linked into the programs a
// release ships (mirrin and mirrin-relay), names each one's licence from its
// licence files, and writes THIRD_PARTY_NOTICES with their full texts.
//
// It asks `go list -deps` which packages each release build links (linux and
// windows without cgo, darwin with it, amd64 and arm64), so modules used only
// by tests or by other platforms are left out, and a module used on one
// platform only is marked as such. A second pass with -tags nowhatsapp marks
// what only the WhatsApp channel brings in.
//
//	go run ./scripts/licenses                          # the summary table
//	go run ./scripts/licenses -o THIRD_PARTY_NOTICES   # write the notices
//	go run ./scripts/licenses -check THIRD_PARTY_NOTICES
//	go run ./scripts/licenses -tags nowhatsapp -mit
//
// -copy names a second file that must be the same as the notices: the copy
// internal/notices embeds in every program, which -o writes too and -check
// compares.
//
// It fails when a module's licence can't be recognised, when a licence that
// isn't allowed shows up, and, with -check, when the file no longer lists what
// the builds link. With -mit it also fails on any GPL-family licence, and on
// MPL code other than the modules in KeptMPL, which is how CI keeps the
// -tags nowhatsapp build one that can be shared under MIT.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Target is one release build.
type Target struct{ GOOS, GOARCH, CGO string }

func (t Target) String() string { return t.GOOS + "/" + t.GOARCH }

// Targets are the builds a release ships, as the Makefile makes them.
var Targets = []Target{
	{"linux", "amd64", "0"}, {"linux", "arm64", "0"},
	{"windows", "amd64", "0"}, {"windows", "arm64", "0"},
	{"darwin", "amd64", "1"}, {"darwin", "arm64", "1"},
}

// Programs are the programs a release ships. The relay is only published for
// Linux, and everything it links is in mirrin too, but it is listed so that a
// module only the relay uses can't go unnoticed.
const Programs = "./cmd/mirrin ./cmd/mirrin-relay"

// WhatsAppTag leaves the WhatsApp channel, and what only it needs, out of a build.
const WhatsAppTag = "nowhatsapp"

// Acknowledged are strong-copyleft modules the project knowingly links, and
// why. Anything else under GPL, LGPL or AGPL stops the check, so a new one is
// a decision rather than an accident (docs/licensing.md).
var Acknowledged = map[string]string{
	"go.mau.fi/libsignal": "the Signal protocol under whatsmeow (WhatsApp); left out by -tags nowhatsapp",
}

// Permissive licences put no conditions on the licence of the whole program.
var Permissive = map[string]bool{
	"MIT": true, "BSD-2-Clause": true, "BSD-3-Clause": true, "Apache-2.0": true, "ISC": true,
	"Zlib": true, "Unlicense": true, "CC0-1.0": true, "public-domain": true,
}

// WeakCopyleft licences cover the files they come with, not the program they
// are linked into; their source must stay available.
var WeakCopyleft = map[string]bool{"MPL-2.0": true}

// KeptMPL are the MPL-2.0 modules the build without WhatsApp knowingly links,
// and why. The MPL covers each one's own files, not the program around them,
// so that build can still be shared under MIT, as long as those files' source
// stays available (every release attaches it). Any other MPL module in that
// build stops -mit, so a new one is a decision rather than an accident.
var KeptMPL = map[string]string{
	"github.com/hashicorp/yamux": "carries the many connections of the relay tunnel (internal/relay, mirrin-relay); used unchanged",
}

// Module is a third-party module linked into at least one build.
type Module struct {
	Path, Version, Dir string
	Licence            string   // SPDX id of its main licence file
	Also               []string // other licences in its licence files (bundled code)
	Files              []File   // licence, notice and patent files, in order
	Builds             []string // targets that link it
	WhatsAppOnly       bool     // linked only because of the WhatsApp channel
}

// File is one licence-related file at the root of a module.
type File struct{ Name, Text string }

func main() {
	out := flag.String("o", "", "write the notices to this file")
	check := flag.String("check", "", "fail if this notices file doesn't list what the builds link")
	tags := flag.String("tags", "", "build tags, as for go build (e.g. "+WhatsAppTag+")")
	mit := flag.Bool("mit", false, "fail unless the build can be shared under MIT: no GPL, LGPL or AGPL code, and no MPL code but KeptMPL")
	versions := flag.Bool("versions", false, "list each module's version (release assets use this)")
	pkg := flag.String("pkg", Programs, "the programs to look at, separated by spaces")
	copyTo := flag.String("copy", "", "with -o, also write the notices here; with -check, fail unless this file is the same as the checked one")
	flag.Parse()

	mods, err := Collect(*pkg, *tags)
	if err != nil {
		fail(err)
	}
	problems := Problems(mods, *mit)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "  "+p)
	}
	if len(problems) > 0 {
		fail(fmt.Errorf("%d licence problem(s). See docs/licensing.md before adding or allowing a licence", len(problems)))
	}
	notices := Notices(mods, *tags, *versions)
	switch {
	case *check != "":
		have, err := os.ReadFile(*check)
		if err != nil {
			fail(err)
		}
		if diff := SummaryDiff(string(have), notices); diff != "" {
			fmt.Fprint(os.Stderr, diff)
			fail(fmt.Errorf("%s no longer lists what the release builds link. Run `make notices` and commit the result", *check))
		}
		if *copyTo != "" {
			if err := sameFile(*check, have, *copyTo); err != nil {
				fail(err)
			}
		}
		if string(have) != notices {
			// Only a licence text changed (a new copyright year, say): the
			// release asset is generated fresh, so this doesn't fail.
			fmt.Printf("::notice::%s lists the right modules, but a licence text changed; `make notices` refreshes it\n", *check)
		}
		fmt.Printf("%s is up to date: %d modules\n", *check, len(mods))
	case *out != "":
		for _, f := range []string{*out, *copyTo} {
			if f == "" {
				continue
			}
			if err := os.WriteFile(f, []byte(notices), 0o644); err != nil {
				fail(err)
			}
			fmt.Printf("wrote %s: %d third-party modules\n", f, len(mods))
		}
	default:
		fmt.Print(Summary(mods, *tags, *versions))
	}
}

// sameFile checks that the file at path holds exactly want, the contents of
// the notices file named name.
func sameFile(name string, want []byte, path string) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s isn't the same as %s. Run `make notices` and commit the result", path, name)
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "licenses:", err)
	os.Exit(1)
}

// listed is one package from `go list -json`.
type listed struct {
	ImportPath string
	Standard   bool
	Module     *listedModule
	Error      *struct{ Err string }
}

type listedModule struct {
	Path, Version, Dir string
	Main               bool
	Replace            *listedModule
}

// deps returns the third-party modules one build of the programs in pkg
// (separated by spaces) links, by path.
func deps(pkg, tags string, t Target) (map[string]listedModule, error) {
	args := []string{"list", "-deps", "-e", "-json=ImportPath,Standard,Module,Error"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, strings.Fields(pkg)...)
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS="+t.GOOS, "GOARCH="+t.GOARCH, "CGO_ENABLED="+t.CGO, "GOFLAGS=-mod=readonly")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list for %s: %v\n%s", t, err, stderr.String())
	}
	mods := map[string]listedModule{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list for %s: %w", t, err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("go list for %s: %s: %s", t, p.ImportPath, p.Error.Err)
		}
		if p.Standard || p.Module == nil || p.Module.Main {
			continue
		}
		m := *p.Module
		if m.Replace != nil {
			m.Version, m.Dir = m.Replace.Version, m.Replace.Dir
		}
		if m.Dir == "" {
			return nil, fmt.Errorf("go list for %s: %s isn't downloaded; run `go mod download`", t, m.Path)
		}
		mods[m.Path] = m
	}
	return mods, nil
}

// Collect finds the modules every release build links, reads their licence
// files and names their licences. With tags "", it also marks the modules only
// the WhatsApp channel brings in.
func Collect(pkg, tags string) ([]*Module, error) {
	byPath := map[string]*Module{}
	without := map[string]bool{} // linked by some build without WhatsApp
	for _, t := range Targets {
		ms, err := deps(pkg, tags, t)
		if err != nil {
			return nil, err
		}
		for path, lm := range ms {
			m := byPath[path]
			if m == nil {
				m = &Module{Path: path, Version: lm.Version, Dir: lm.Dir}
				byPath[path] = m
			}
			m.Builds = append(m.Builds, t.String())
		}
		if tags == "" {
			ms, err := deps(pkg, WhatsAppTag, t)
			if err != nil {
				return nil, err
			}
			for path := range ms {
				without[path] = true
			}
		}
	}
	var mods []*Module
	for _, m := range byPath {
		m.WhatsAppOnly = tags == "" && !without[m.Path]
		files, err := licenceFiles(m.Dir)
		if err != nil {
			return nil, err
		}
		m.Files = files
		m.Licence, m.Also = classifyFiles(files)
		mods = append(mods, m)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	return mods, nil
}

// licenceName matches licence, notice and patent files at a module's root:
// LICENSE, LICENSE.txt, LICENSE-SQLITE, COPYING, NOTICE, PATENTS, License.txt...
var licenceName = regexp.MustCompile(`(?i)^(un)?licen[cs]e|^copying|^notice|^patents|^third_party_licenses`)

// mainLicence matches the file that holds a module's own licence.
var mainLicence = regexp.MustCompile(`(?i)^((un)?licen[cs]e|copying)(\.(txt|md))?$`)

func licenceFiles(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		if e.IsDir() || !licenceName.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, File{Name: e.Name(), Text: strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))})
	}
	// The module's own licence first, then the rest by name.
	sort.SliceStable(files, func(i, j int) bool {
		mi, mj := mainLicence.MatchString(files[i].Name), mainLicence.MatchString(files[j].Name)
		if mi != mj {
			return mi
		}
		return files[i].Name < files[j].Name
	})
	return files, nil
}

// classifyFiles names the licence of a module's main licence file and any
// others its licence files mention for code bundled alongside. NOTICE and
// PATENTS files add terms but aren't licences.
func classifyFiles(files []File) (main string, also []string) {
	main = "unknown"
	seen := map[string]bool{}
	for i, f := range files {
		up := strings.ToUpper(f.Name)
		if strings.HasPrefix(up, "NOTICE") || strings.HasPrefix(up, "PATENTS") {
			continue
		}
		ids := ClassifyAll(f.Text)
		if i == 0 && mainLicence.MatchString(f.Name) {
			main = Classify(f.Text)
			seen[main] = true
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				also = append(also, id)
			}
		}
	}
	sort.Strings(also)
	return main, also
}

var space = regexp.MustCompile(`[\s*#>]+`)

// licences are the wordings each licence is known by, most specific first.
// The copyleft licences name each other (the MPL lists the GPL family as
// secondary licences, the GPL mentions the AGPL in section 13, the LGPL
// builds on the GPL), so the GPL family is told apart by its title, which is
// the first thing in its text.
var licences = []struct {
	id    string
	match func(has, starts func(string) bool) bool
}{
	{"MPL-2.0", func(has, _ func(string) bool) bool {
		return has("mozilla public license version 2.0") || has("mozilla public license, version 2.0") || has("mozilla public license, v. 2.0")
	}},
	{"AGPL-3.0", func(_, starts func(string) bool) bool { return starts("gnu affero general public license") }},
	{"LGPL-3.0", func(_, starts func(string) bool) bool { return starts("gnu lesser general public license version 3") }},
	{"LGPL-2.1", func(_, starts func(string) bool) bool {
		return starts("gnu lesser general public license") || starts("gnu library general public license")
	}},
	{"GPL-3.0", func(_, starts func(string) bool) bool { return starts("gnu general public license version 3") }},
	{"GPL-2.0", func(_, starts func(string) bool) bool { return starts("gnu general public license") }},
	{"Apache-2.0", func(has, _ func(string) bool) bool { return has("apache license") && has("version 2.0") }},
	{"MIT", func(has, _ func(string) bool) bool {
		return has("permission is hereby granted, free of charge, to any person obtaining a copy")
	}},
	{"BSD-3-Clause", func(has, _ func(string) bool) bool {
		return has("redistribution and use in source and binary forms") &&
			(has("neither the name") || has("may not be used to endorse or promote") || has("names of its contributors may not be used"))
	}},
	{"BSD-2-Clause", func(has, _ func(string) bool) bool {
		return has("redistribution and use in source and binary forms") &&
			!(has("neither the name") || has("may not be used to endorse or promote") || has("names of its contributors may not be used"))
	}},
	{"ISC", func(has, _ func(string) bool) bool {
		return has("permission to use, copy, modify, and/or distribute this software for any purpose") ||
			has("permission to use, copy, modify, and distribute this software for any purpose with or without fee")
	}},
	{"Unlicense", func(has, _ func(string) bool) bool {
		return has("this is free and unencumbered software released into the public domain")
	}},
	{"CC0-1.0", func(has, _ func(string) bool) bool { return has("cc0 1.0 universal") || has("creative commons zero") }},
	{"Zlib", func(has, _ func(string) bool) bool {
		return has("provided 'as-is', without any express or implied warranty") && has("altered source versions must be plainly marked")
	}},
	{"public-domain", func(has, _ func(string) bool) bool {
		return has("is public domain") || has("dedicated to the public domain") || has("released into the public domain") ||
			has("placed in the public domain")
	}},
}

// Classify names the licence a text is, by the wording each licence is known
// by, or "unknown".
func Classify(text string) string {
	if ids := classify(text, true); len(ids) > 0 {
		return ids[0]
	}
	return "unknown"
}

// ClassifyAll names every licence a text holds, for files that collect the
// licences of bundled code. A copyleft licence's own text names others, so it
// is reported alone.
func ClassifyAll(text string) []string { return classify(text, false) }

func classify(text string, first bool) []string {
	t := strings.TrimSpace(space.ReplaceAllString(strings.ToLower(text), " "))
	has := func(s string) bool { return strings.Contains(t, s) }
	head := t[:min(len(t), 160)]
	starts := func(s string) bool { return strings.Contains(head, s) }
	var ids []string
	for i, l := range licences {
		if !l.match(has, starts) {
			continue
		}
		if first || i < 6 { // MPL and the GPL family: the text is that one licence
			return []string{l.id}
		}
		if l.id == "public-domain" && len(ids) > 0 {
			continue // "public domain" in a disclaimer of a licence already found
		}
		ids = append(ids, l.id)
	}
	return ids
}

// Problems lists modules whose licence is unknown or not allowed. With mit,
// the build must be one that can be shared under MIT: no GPL-family code, and
// MPL code only from the modules in KeptMPL.
func Problems(mods []*Module, mit bool) []string {
	var out []string
	for _, m := range mods {
		for _, id := range append([]string{m.Licence}, m.Also...) {
			switch {
			case id == "unknown":
				out = append(out, fmt.Sprintf("%s: can't tell its licence from %s", m.Path, fileNames(m.Files)))
			case Permissive[id]:
			case WeakCopyleft[id] && (!mit || KeptMPL[m.Path] != ""):
			case Acknowledged[m.Path] != "" && !mit:
			case mit && WeakCopyleft[id]:
				out = append(out, fmt.Sprintf("%s is %s, which this build hasn't agreed to link (see KeptMPL in scripts/licenses)", m.Path, id))
			case mit:
				out = append(out, fmt.Sprintf("%s is %s, which this build must not link", m.Path, id))
			default:
				out = append(out, fmt.Sprintf("%s is %s, which the project hasn't agreed to link (see Acknowledged in scripts/licenses)", m.Path, id))
			}
		}
	}
	return out
}

func fileNames(files []File) string {
	if len(files) == 0 {
		return "no licence file"
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	return strings.Join(names, ", ")
}

// goLicence is the licence of the Go standard library and runtime, which
// every Go program contains (https://go.dev/LICENSE). It is kept here rather
// than read from GOROOT, which some Go installs (Homebrew's) leave it out of.
const goLicence = `Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.`

// Other is third-party material in the programs that isn't a Go module, so
// Collect can't find it.
type Other struct{ Name, Licence, URL, About, Text string }

// OtherMaterial is what the programs carry besides Go modules, with its
// licence. Add to it when something written elsewhere is embedded.
var OtherMaterial = []Other{{
	Name: "BIP-39 English wordlist", Licence: "MIT",
	URL:   "https://github.com/bitcoin/bips/blob/master/bip-0039/english.txt",
	About: "The 12 words that recover an encrypted backup come from this list (internal/backup/bip39_english.txt, unchanged). BIP-39 is published under the MIT licence, and the same list ships in its reference implementation, https://github.com/trezor/python-mnemonic, under this notice:",
	Text: `The MIT License (MIT)

Copyright (c) 2013-2016 Pavol Rusnak

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.`,
}}

// builds says which release builds link a module.
func builds(m *Module) string {
	var s string
	if len(m.Builds) == len(Targets) {
		s = "all"
	} else {
		byOS := map[string][]string{}
		var oses []string
		for _, b := range m.Builds {
			goos, arch, _ := strings.Cut(b, "/")
			if byOS[goos] == nil {
				oses = append(oses, goos)
			}
			byOS[goos] = append(byOS[goos], arch)
		}
		sort.Strings(oses)
		var parts []string
		for _, o := range oses {
			if len(byOS[o]) == 2 {
				parts = append(parts, o)
			} else {
				parts = append(parts, o+"/"+strings.Join(byOS[o], ","))
			}
		}
		s = strings.Join(parts, ", ")
	}
	if m.WhatsAppOnly {
		s += " (WhatsApp)"
	}
	return s
}

func licenceLabel(m *Module) string {
	if len(m.Also) == 0 {
		return m.Licence
	}
	return m.Licence + " (bundles " + strings.Join(m.Also, ", ") + ")"
}

const summaryStart, summaryEnd = "--- modules ---", "--- end of modules ---"

// Summary is the table of modules: path, licence and the builds that link it.
func Summary(mods []*Module, tags string, versions bool) string {
	var b strings.Builder
	w := 0
	for _, m := range mods {
		if n := len(name(m, versions)); n > w {
			w = n
		}
	}
	fmt.Fprintln(&b, summaryStart)
	fmt.Fprintf(&b, "%-*s  %-34s  %s\n", w, "Module", "Licence", "Linked into")
	for _, m := range mods {
		fmt.Fprintf(&b, "%-*s  %-34s  %s\n", w, name(m, versions), licenceLabel(m), builds(m))
	}
	fmt.Fprintln(&b, summaryEnd)
	return b.String()
}

func name(m *Module, versions bool) string {
	if versions && m.Version != "" {
		return m.Path + " " + m.Version
	}
	return m.Path
}

// Terms says in plain words what licence each build is under, from what it
// links: GPL code makes the whole program GPL, MPL code keeps only its own
// files under the MPL, and a permissive licence asks only for its notice.
// With tags "", it describes both the default build and the one without
// WhatsApp; with tags, the one build the list is for.
func Terms(mods []*Module, tags string) []string {
	var whatsApp, gpl, mpl []string // only for WhatsApp; GPL and MPL in every build
	whatsAppGPL, gplIDs := false, map[string]bool{}
	for _, m := range mods {
		for _, id := range append([]string{m.Licence}, m.Also...) {
			isGPL := strings.HasPrefix(id, "GPL-") || strings.HasPrefix(id, "AGPL-")
			if !isGPL && !WeakCopyleft[id] {
				continue
			}
			named := m.Path + " (" + id + ")"
			if isGPL {
				gplIDs[id] = true
			}
			switch {
			case m.WhatsAppOnly:
				whatsApp = append(whatsApp, named)
				whatsAppGPL = whatsAppGPL || isGPL
			case isGPL:
				gpl = append(gpl, named)
			default:
				mpl = append(mpl, named)
			}
		}
	}
	gplName := "the GPL"
	if len(gplIDs) == 1 {
		for id := range gplIDs {
			gplName = id
		}
	}
	var out []string
	subject, contains := "Every program", "Every program contains "
	if tags != "" {
		subject, contains = "A mirrin program built with -tags "+tags, "It contains "
	} else if len(whatsApp) > 0 {
		s := "The default mirrin programs include the WhatsApp channel, which links " + list(whatsApp)
		if whatsAppGPL {
			s += ", so they are distributed as a whole under " + gplName
		}
		out = append(out, s+".")
		subject = "A mirrin program built with -tags " + WhatsAppTag + " leaves out everything marked (WhatsApp) below and"
		contains = "Builds with and without WhatsApp both contain "
	}
	if len(gpl) > 0 {
		out = append(out, subject+" links "+list(gpl)+", so it is distributed as a whole under "+gplName+".")
	} else {
		out = append(out, subject+" is distributed under MIT.")
	}
	if len(mpl) > 0 {
		own := "that module's own files"
		if len(mpl) > 1 {
			own = "those modules' own files"
		}
		out = append(out, contains+list(mpl)+". The MPL is a file-level copyleft: "+own+" stay under it, with their source available, and it asks nothing of the rest of the program.")
	}
	return append(out, "Every other module is under a permissive licence, which asks only that its notice goes with the program, as it does here.")
}

// list joins items as a sentence would: "a", "a and b", "a, b and c".
func list(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// Notices is the whole THIRD_PARTY_NOTICES file.
func Notices(mods []*Module, tags string, versions bool) string {
	var b strings.Builder
	rule := strings.Repeat("=", 78)
	b.WriteString("Mirrin third-party notices\n==========================\n\n")
	b.WriteString(wrap(`Mirrin's own source code is under the MIT licence (LICENSE). The programs built from it also contain the Go standard library, the third-party modules listed here and the other material listed after them, each under its own licence, whose full texts follow.`))
	b.WriteString("\n")
	for _, p := range Terms(mods, tags) {
		b.WriteString(wrap(p))
		b.WriteString("\n")
	}
	b.WriteString(wrap("docs/licensing.md explains what that means for the programs published with each release."))
	b.WriteString("\n")
	build := "mirrin for Linux, Windows and macOS (amd64 and arm64), and mirrin-relay for Linux"
	if tags != "" {
		build += ", built with -tags " + tags
	}
	b.WriteString(wrap("This list covers " + build + `. "(WhatsApp)" marks what only the WhatsApp channel needs: a build made with -tags ` + WhatsAppTag + ` leaves it out.`))
	b.WriteString("\n")
	b.WriteString(wrap("Generated by `go run ./scripts/licenses -o THIRD_PARTY_NOTICES` (`make notices`); please don't edit it by hand."))
	b.WriteString("\n")
	b.WriteString(Summary(mods, tags, versions))
	fmt.Fprintf(&b, "\n%s\nGo standard library and runtime (BSD-3-Clause)\nhttps://go.dev/LICENSE\n%s\n\n%s\n", rule, strings.Repeat("-", 78), goLicence)
	for _, o := range OtherMaterial {
		fmt.Fprintf(&b, "\n%s\n%s (%s)\n%s\n%s\n\n%s\n%s\n", rule, o.Name, o.Licence, o.URL, strings.Repeat("-", 78), wrap(o.About), o.Text)
	}
	for _, m := range mods {
		fmt.Fprintf(&b, "\n%s\n%s (%s)\nhttps://pkg.go.dev/%s\n%s\n", rule, name(m, versions), licenceLabel(m), m.Path, strings.Repeat("-", 78))
		for i, f := range m.Files {
			if i > 0 {
				fmt.Fprintf(&b, "\n--- %s ---\n", f.Name)
			}
			fmt.Fprintf(&b, "\n%s\n", f.Text)
		}
	}
	return b.String()
}

// wrap folds a paragraph at 78 columns.
func wrap(s string) string {
	var b strings.Builder
	n := 0
	for _, w := range strings.Fields(s) {
		if n > 0 && n+1+len(w) > 78 {
			b.WriteString("\n")
			n = 0
		} else if n > 0 {
			b.WriteString(" ")
			n++
		}
		b.WriteString(w)
		n += len(w)
	}
	return b.String() + "\n"
}

// summaryLines are the table rows of a notices file, with spacing normalised.
func summaryLines(s string) []string {
	start := strings.Index(s, summaryStart)
	end := strings.Index(s, summaryEnd)
	if start < 0 || end < start {
		return nil
	}
	var out []string
	for _, l := range strings.Split(s[start+len(summaryStart):end], "\n") {
		if f := strings.Join(strings.Fields(l), " "); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// SummaryDiff describes how the module table in have differs from want, or
// returns "" when they list the same modules, licences and builds.
func SummaryDiff(have, want string) string {
	h, w := summaryLines(have), summaryLines(want)
	if h == nil {
		return "  the file has no module table\n"
	}
	inH := map[string]bool{}
	for _, l := range h {
		inH[l] = true
	}
	inW := map[string]bool{}
	for _, l := range w {
		inW[l] = true
	}
	var b strings.Builder
	for _, l := range w {
		if !inH[l] {
			fmt.Fprintf(&b, "  + %s\n", l)
		}
	}
	for _, l := range h {
		if !inW[l] {
			fmt.Fprintf(&b, "  - %s\n", l)
		}
	}
	return b.String()
}
