package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// packFake has one installed pack, pinned to a commit, with an update.
type packFake struct {
	*fake
	applied []string
}

func (p *packFake) InstalledPacks(context.Context) []PackInfo {
	return []PackInfo{{Dir: "news", Name: "news", Installed: true}}
}
func (p *packFake) PackCommits(context.Context) map[string]string {
	return map[string]string{"news": "1111111111111111111111111111111111111111"}
}
func (p *packFake) CheckPackUpdates(context.Context) ([]PackUpdate, error) {
	return []PackUpdate{{Dir: "news", From: "1111111111111111111111111111111111111111", To: "2222222222222222222222222222222222222222", Pending: true, Changes: []string{"changed protocols/news.yaml"}}}, nil
}
func (p *packFake) ApplyPackUpdate(_ context.Context, dir string) error {
	p.applied = append(p.applied, dir)
	return nil
}
func (p *packFake) PreviewPack(_ context.Context, name string) (PackPreview, error) {
	return PackPreview{Name: name, Protocols: []ProtocolInfo{{Name: "news digest", Schedule: "0 7 * * *", Requires: []string{"web"}, Prompt: "Read the front pages."}}}, nil
}

// The store shows the commit each pack is pinned to, whether an update is
// waiting and what it changes, applies it on request, and previews a pack
// before it is installed.
func TestProtocolsPinsUpdatesAndPreview(t *testing.T) {
	e := newEnv(t)
	pf := &packFake{fake: e.f}
	e.s.WithProtocols(pf)
	w := e.do(onLoopback, req{path: "/protocols/installed", header: bearer(master)})
	var inst struct{ Packs []PackInfo }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &inst) != nil || len(inst.Packs) != 1 || inst.Packs[0].Commit != "1111111111111111111111111111111111111111" || inst.Packs[0].Update != nil {
		t.Fatalf("installed: %d %s", w.Code, w.Body)
	}
	w = e.do(onLoopback, req{method: "POST", path: "/protocols/check-updates", header: bearer(master)})
	var chk struct{ Packs []PackInfo }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &chk) != nil || len(chk.Packs) != 1 {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	if p := chk.Packs[0]; p.Commit == "" || p.Update == nil || !p.Update.Pending || p.Update.To != "2222222222222222222222222222222222222222" || len(p.Update.Changes) != 1 {
		t.Fatalf("check: %+v", p)
	}
	if w := e.do(onLoopback, req{method: "POST", path: "/protocols/apply-update", body: `{"dir":"news"}`, header: bearer(master)}); w.Code != 200 || len(pf.applied) != 1 || pf.applied[0] != "news" {
		t.Fatalf("apply: %d %s %v", w.Code, w.Body, pf.applied)
	}
	w = e.do(onLoopback, req{method: "POST", path: "/protocols/preview", body: `{"name":"news"}`, header: bearer(master)})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"prompt":"Read the front pages."`) || !strings.Contains(w.Body.String(), `"schedule":"0 7 * * *"`) {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	// settings: only on this computer
	if w := e.do(onRemote, req{method: "POST", path: "/protocols/check-updates", header: bearer(master)}); w.Code != 404 {
		t.Fatalf("check-updates from another device: %d", w.Code)
	}
}

// The Routines page's switch, Skip next and time field reach the twin with
// the protocol's name as it is, spaces and all; a change without what to
// change is turned away.
func TestProtocolSwitchSkipAndTime(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		path, body string
		code       int
		saw        string
	}{
		{"/protocols/morning%20briefing/enabled", `{"enabled":false}`, 200, "enabled morning briefing false"},
		{"/protocols/morning%20briefing/skip", "", 200, "skip morning briefing"},
		{"/protocols/morning%20briefing/schedule", `{"schedule":"30 8 * * 1-5"}`, 200, "schedule morning briefing 30 8 * * 1-5"},
		{"/protocols/morning%20briefing/enabled", `{}`, 400, ""},
		{"/protocols/morning%20briefing/schedule", `{"schedule":" "}`, 400, ""},
	} {
		w := e.do(onLoopback, req{method: "POST", path: c.path, body: c.body, header: bearer(master)})
		if w.Code != c.code {
			t.Errorf("%s %s: %d %s", c.path, c.body, w.Code, w.Body)
		}
		if c.saw != "" && !e.f.saw(c.saw) {
			t.Errorf("%s: the twin wasn't asked %q", c.path, c.saw)
		}
	}
}
