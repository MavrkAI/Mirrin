package api

import (
	"encoding/json"
	"testing"
)

// The memory page learns that the portrait was set aside after a forget, so
// it can say why there isn't one.
func TestMemoryPortraitSaysWhenItWasSetAside(t *testing.T) {
	e := newEnv(t)
	get := func() map[string]any {
		w := e.do(onLoopback, req{path: "/memory/portrait", header: bearer(master)})
		var got map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("GET /memory/portrait: %d %s", w.Code, w.Body)
		}
		return got
	}
	if got := get(); got["aside"] != false {
		t.Fatalf("no forget yet, but aside is %v", got["aside"])
	}
	e.f.aside = true
	if got := get(); got["aside"] != true || got["text"] != "" {
		t.Fatalf("after a forget: %v", got)
	}
}
