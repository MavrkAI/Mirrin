package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	gpl3 = `                    GNU GENERAL PUBLIC LICENSE
                       Version 3, 29 June 2007
 Copyright (C) 2007 Free Software Foundation, Inc. <http://fsf.org/>
  13. Use with the GNU Affero General Public License.
  Notwithstanding any other provision of this License...`
	agpl3 = `                    GNU AFFERO GENERAL PUBLIC LICENSE
                       Version 3, 19 November 2007
  13. Remote Network Interaction; Use with the GNU General Public License.`
	lgpl3 = `                   GNU LESSER GENERAL PUBLIC LICENSE
                       Version 3, 29 June 2007
  This version of the GNU Lesser General Public License incorporates
the terms and conditions of version 3 of the GNU General Public License`
	mpl2 = `Mozilla Public License Version 2.0
==================================
1.12. "Secondary License"
    means either the GNU General Public License, Version 2.0, the GNU
    Lesser General Public License, Version 2.1, the GNU Affero General
    Public License, Version 3.0, or any later versions of those licenses`
	mit = `MIT License

Copyright (c) 2020 Someone

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software")`
	bsd3 = `Copyright (c) 2009 The Go Authors. All rights reserved.
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from`
	bsd2 = `Copyright (c) 2013, Georg Reinke
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions
are met:
1. Redistributions of source code must retain the above copyright`
	apache = `                                 Apache License
                           Version 2.0, January 2004
                        http://www.apache.org/licenses/`
	isc = `Copyright (c) 2023 Anmol Sethi <hi@nhooyr.io>

Permission to use, copy, modify, and distribute this software for any
purpose with or without fee is hereby granted, provided that the above`
	zlib = `This software is provided 'as-is', without any express or implied
warranty. In no event will the authors be held liable for any damages
3. Altered source versions must be plainly marked as such, and must not be
misrepresented as being the original software.`
	sqlite = `SQLite Is Public Domain

All of the code and documentation in SQLite has been dedicated to the public
domain by the authors.`
)

// The GPL mentions the AGPL (section 13), the AGPL and LGPL mention the GPL,
// and the MPL names all three as secondary licences. go.mau.fi/libsignal was
// once reported as AGPL-3.0 because of that.
func TestClassify(t *testing.T) {
	cases := map[string]string{
		gpl3: "GPL-3.0", agpl3: "AGPL-3.0", lgpl3: "LGPL-3.0", mpl2: "MPL-2.0",
		mit: "MIT", bsd3: "BSD-3-Clause", bsd2: "BSD-2-Clause", apache: "Apache-2.0",
		isc: "ISC", zlib: "Zlib", sqlite: "public-domain", "All rights reserved.": "unknown",
	}
	for text, want := range cases {
		if got := Classify(text); got != want {
			t.Errorf("Classify(%.40q) = %s, want %s", text, got, want)
		}
	}
}

// A file that collects the licences of bundled code names each of them; a
// copyleft licence's own text is only that licence.
func TestClassifyAll(t *testing.T) {
	if got := strings.Join(ClassifyAll(apache+"\n\n"+mit+"\n\n"+bsd3), ","); got != "Apache-2.0,MIT,BSD-3-Clause" {
		t.Errorf("bundled = %s", got)
	}
	if got := strings.Join(ClassifyAll(mpl2), ","); got != "MPL-2.0" {
		t.Errorf("MPL = %s", got)
	}
	if got := strings.Join(ClassifyAll(gpl3), ","); got != "GPL-3.0" {
		t.Errorf("GPL = %s", got)
	}
}

func TestClassifyFiles(t *testing.T) {
	main, also := classifyFiles([]File{
		{Name: "LICENSE", Text: bsd3},
		{Name: "LICENSE-SQLITE", Text: sqlite},
		{Name: "LICENSE-VEC", Text: mit},
		{Name: "NOTICE", Text: "Some notice that mentions the public domain"},
	})
	if main != "BSD-3-Clause" || strings.Join(also, ",") != "MIT,public-domain" {
		t.Fatalf("main %s, also %v", main, also)
	}
	if main, _ := classifyFiles(nil); main != "unknown" {
		t.Fatalf("no files: %s", main)
	}
}

func TestProblems(t *testing.T) {
	mods := []*Module{
		{Path: "example.com/ok", Licence: "MIT"},
		{Path: "example.com/weak", Licence: "MPL-2.0"},
		{Path: "go.mau.fi/libsignal", Licence: "GPL-3.0"},
		{Path: "example.com/gpl", Licence: "GPL-3.0"},
		{Path: "example.com/mystery", Licence: "unknown", Files: []File{{Name: "COPYING"}}},
		{Path: "example.com/bundles", Licence: "MIT", Also: []string{"LGPL-2.1"}},
	}
	got := strings.Join(Problems(mods, false), "\n")
	for _, want := range []string{"example.com/gpl is GPL-3.0", "example.com/mystery: can't tell its licence from COPYING", "example.com/bundles is LGPL-2.1"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
	for _, allowed := range []string{"example.com/ok", "example.com/weak", "go.mau.fi/libsignal"} {
		if strings.Contains(got, allowed+" ") {
			t.Errorf("%s was refused:\n%s", allowed, got)
		}
	}
	strict := strings.Join(Problems(append(mods, &Module{Path: "github.com/hashicorp/yamux", Licence: "MPL-2.0"}), true), "\n")
	for _, want := range []string{"example.com/weak is MPL-2.0, which this build hasn't agreed to link", "go.mau.fi/libsignal is GPL-3.0"} {
		if !strings.Contains(strict, want) {
			t.Errorf("-mit: want %q in:\n%s", want, strict)
		}
	}
	if strings.Contains(strict, "yamux") {
		t.Errorf("-mit refused an MPL module in KeptMPL:\n%s", strict)
	}
}

// The notices say plainly what each build is: GPL-3.0 as a whole with
// WhatsApp, MIT without it, and the MPL files that are in both.
func TestTerms(t *testing.T) {
	mods := []*Module{
		{Path: "example.com/ok", Licence: "MIT"},
		{Path: "github.com/hashicorp/yamux", Licence: "MPL-2.0"},
		{Path: "go.mau.fi/libsignal", Licence: "GPL-3.0", WhatsAppOnly: true},
		{Path: "go.mau.fi/whatsmeow", Licence: "MPL-2.0", WhatsAppOnly: true},
	}
	got := strings.Join(Terms(mods, ""), "\n")
	for _, want := range []string{
		"The default mirrin programs include the WhatsApp channel, which links go.mau.fi/libsignal (GPL-3.0) and go.mau.fi/whatsmeow (MPL-2.0), so they are distributed as a whole under GPL-3.0.",
		"A mirrin program built with -tags nowhatsapp leaves out everything marked (WhatsApp) below and is distributed under MIT.",
		"Builds with and without WhatsApp both contain github.com/hashicorp/yamux (MPL-2.0). The MPL is a file-level copyleft: that module's own files stay under it",
		"Every other module is under a permissive licence",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
	// Listing the build without WhatsApp: no WhatsApp modules are in it.
	got = strings.Join(Terms(mods[:2], WhatsAppTag), "\n")
	for _, want := range []string{"A mirrin program built with -tags nowhatsapp is distributed under MIT.", "It contains github.com/hashicorp/yamux (MPL-2.0)."} {
		if !strings.Contains(got, want) {
			t.Errorf("-tags %s: want %q in:\n%s", WhatsAppTag, want, got)
		}
	}
	// A GPL module every build links makes every build GPL, and they say so.
	got = strings.Join(Terms([]*Module{{Path: "example.com/gpl", Licence: "GPL-2.0"}}, ""), "\n")
	if !strings.Contains(got, "Every program links example.com/gpl (GPL-2.0), so it is distributed as a whole under GPL-2.0.") || strings.Contains(got, "MIT") {
		t.Errorf("GPL in every build:\n%s", got)
	}
}

func TestBuildsLabel(t *testing.T) {
	all := &Module{}
	for _, t := range Targets {
		all.Builds = append(all.Builds, t.String())
	}
	if got := builds(all); got != "all" {
		t.Errorf("all = %q", got)
	}
	some := &Module{Builds: []string{"darwin/amd64", "darwin/arm64", "windows/arm64"}, WhatsAppOnly: true}
	if got := builds(some); got != "darwin, windows/arm64 (WhatsApp)" {
		t.Errorf("some = %q", got)
	}
}

// -check fails when a module, a licence or the builds that link it changed,
// and ignores the licence texts, which change with copyright years.
func TestSummaryDiff(t *testing.T) {
	a := []*Module{{Path: "example.com/a", Licence: "MIT", Builds: []string{"linux/amd64"}, Files: []File{{Name: "LICENSE", Text: mit}}}}
	have := Notices(a, "", false)
	if d := SummaryDiff(have, have); d != "" {
		t.Fatalf("same file differs: %s", d)
	}
	a[0].Files[0].Text = strings.Replace(mit, "2020", "2026", 1)
	if d := SummaryDiff(have, Notices(a, "", false)); d != "" {
		t.Fatalf("a new copyright year counts as a change: %s", d)
	}
	b := append(a, &Module{Path: "example.com/b", Licence: "MPL-2.0", Builds: []string{"linux/amd64"}})
	d := SummaryDiff(have, Notices(b, "", false))
	if !strings.Contains(d, "+ example.com/b MPL-2.0 linux/amd64") {
		t.Fatalf("a new module isn't reported:\n%s", d)
	}
	if d := SummaryDiff("hand-written", have); !strings.Contains(d, "no module table") {
		t.Fatalf("a file without the table: %q", d)
	}
}

// The WhatsApp channel is what links the GPL-3.0 Signal library, and
// -tags nowhatsapp leaves it out. This asks the Go tool about the real build.
func TestNoWhatsAppLeavesTheSignalLibraryOut(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	linux := Target{"linux", "amd64", "0"}
	with, err := deps("../../cmd/mirrin", "", linux)
	if err != nil {
		t.Fatal(err)
	}
	without, err := deps("../../cmd/mirrin", WhatsAppTag, linux)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := with["go.mau.fi/libsignal"]; !ok {
		t.Fatal("the default build doesn't link go.mau.fi/libsignal any more; update docs/licensing.md and Acknowledged")
	}
	for path := range without {
		if strings.HasPrefix(path, "go.mau.fi/") {
			t.Errorf("-tags %s still links %s", WhatsAppTag, path)
		}
	}
}

// THIRD_PARTY_NOTICES lists every module the released programs link, with or
// without WhatsApp. github.com/hashicorp/yamux, which the relay tunnel
// brought in, was once linked but not listed.
func TestEveryLinkedModuleIsListed(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	b, err := os.ReadFile("../../THIRD_PARTY_NOTICES")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]string{}
	for _, l := range summaryLines(string(b)) {
		if f := strings.Fields(l); len(f) > 1 {
			listed[f[0]] = f[1]
		}
	}
	programs := strings.ReplaceAll(Programs, "./", "../../")
	for _, tags := range []string{"", WhatsAppTag} {
		linked, err := deps(programs, tags, Target{"linux", "amd64", "0"})
		if err != nil {
			t.Fatal(err)
		}
		for path := range linked {
			if listed[path] == "" {
				t.Errorf("-tags %q links %s, which THIRD_PARTY_NOTICES doesn't list. Run `make notices`", tags, path)
			}
		}
	}
	if listed["github.com/hashicorp/yamux"] != "MPL-2.0" {
		t.Errorf("github.com/hashicorp/yamux is listed as %q", listed["github.com/hashicorp/yamux"])
	}
}

// The build without WhatsApp can be shared under MIT: it links no GPL code,
// and no MPL code but what KeptMPL names. This reads the real modules.
func TestNoWhatsAppBuildIsMIT(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	linked, err := deps(strings.ReplaceAll(Programs, "./", "../../"), WhatsAppTag, Target{"linux", "amd64", "0"})
	if err != nil {
		t.Fatal(err)
	}
	var mods []*Module
	for path, lm := range linked {
		files, err := licenceFiles(lm.Dir)
		if err != nil {
			t.Fatal(err)
		}
		m := &Module{Path: path, Files: files}
		m.Licence, m.Also = classifyFiles(files)
		mods = append(mods, m)
	}
	for _, p := range Problems(mods, true) {
		t.Error(p)
	}
	for path := range KeptMPL {
		if _, ok := linked[path]; !ok {
			t.Errorf("%s is in KeptMPL but the build without WhatsApp doesn't link it any more; take it out and update docs/licensing.md", path)
		}
	}
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "THIRD_PARTY_NOTICES")
	if err := os.WriteFile(copyPath, []byte("notices\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sameFile("THIRD_PARTY_NOTICES", []byte("notices\n"), copyPath); err != nil {
		t.Fatal(err)
	}
	err := sameFile("THIRD_PARTY_NOTICES", []byte("notices, changed\n"), copyPath)
	if err == nil || !strings.Contains(err.Error(), "make notices") {
		t.Fatalf("err = %v", err)
	}
	if err := sameFile("THIRD_PARTY_NOTICES", nil, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing copy passed")
	}
}

// The BIP-39 wordlist is compiled into every program, but it isn't a Go
// module, so its MIT notice was missing from THIRD_PARTY_NOTICES.
func TestOtherMaterialIsListed(t *testing.T) {
	b, err := os.ReadFile("../../THIRD_PARTY_NOTICES")
	if err != nil {
		t.Fatal(err)
	}
	fresh := Notices(nil, "", false)
	for _, o := range OtherMaterial {
		head := o.Name + " (" + o.Licence + ")"
		if !strings.Contains(fresh, head) || !strings.Contains(fresh, o.Text) {
			t.Errorf("the notices leave out %s", head)
		}
		if !strings.Contains(string(b), head) || !strings.Contains(string(b), o.Text) {
			t.Errorf("THIRD_PARTY_NOTICES leaves out %s. Run `make notices`", head)
		}
	}
	if !strings.Contains(fresh, "BIP-39 English wordlist (MIT)") || !strings.Contains(fresh, "Copyright (c) 2013-2016 Pavol Rusnak") {
		t.Error("the notices don't carry the BIP-39 wordlist's MIT notice")
	}
	if _, err := os.Stat("../../internal/backup/bip39_english.txt"); err != nil {
		t.Errorf("the notice names internal/backup/bip39_english.txt, which has moved: %v", err)
	}
}
