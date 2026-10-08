package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// listenAs opens /events on this computer with token tok until stop, and
// returns the first frame it said.
func listenAs(t *testing.T, e *env, path, tok string) (first string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), listenerKey, listener{kind: kindLoopback, via: "loopback"}))
	r := httptest.NewRequest("GET", "http://127.0.0.1:7742"+path, nil).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:50003"
	r.Header.Set("Authorization", "Bearer "+tok)
	w := &streamWriter{h: map[string][]string{}}
	done := make(chan struct{})
	go func() { defer close(done); e.s.Handler().ServeHTTP(w, r) }()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(w.String(), "\n\n") {
		if time.Now().After(deadline) {
			t.Fatalf("%s never listened: %s", path, w.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	first = strings.TrimPrefix(strings.SplitN(w.String(), "\n\n", 2)[0], "data: ")
	return first, func() { cancel(); <-done }
}

// A screen here says whether the owner can see it: as it connects, and when
// it changes. A background tab is open but not in sight, so a page handed
// over by voice still brings a screen up; a wall screen on this computer
// isn't one anybody sits at, so it doesn't count at all.
func TestAScreenSaysWhetherItIsInSight(t *testing.T) {
	e := newEnv(t)
	bus := e.f.bus

	first, hidden := listenAs(t, e, "/events?hidden=1", master)
	var ev struct{ Kind, Screen string }
	if err := json.Unmarshal([]byte(first), &ev); err != nil || ev.Kind != "state" || ev.Screen == "" {
		t.Fatalf("first frame %q: %v", first, err)
	}
	if bus.ScreensOpen() != 1 || bus.ScreensInSight() != 0 {
		t.Fatalf("a background tab: open %d, in sight %d", bus.ScreensOpen(), bus.ScreensInSight())
	}
	seen := func(id string, visible bool) int {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"screen": id, "visible": visible})
		return e.do(onLoopback, req{method: "POST", path: "/events/seen", body: string(b), header: bearer(master)}).Code
	}
	if c := seen(ev.Screen, true); c != 204 || bus.ScreensInSight() != 1 {
		t.Fatalf("came into sight: %d, %d in sight", c, bus.ScreensInSight())
	}
	if c := seen(ev.Screen, false); c != 204 || bus.ScreensInSight() != 0 {
		t.Fatalf("went out of sight: %d, %d in sight", c, bus.ScreensInSight())
	}
	if c := seen("someone-else", true); c != 404 || bus.ScreensInSight() != 0 {
		t.Fatalf("an unknown screen: %d", c)
	}
	if c := e.do(onLoopback, req{method: "POST", path: "/events/seen", body: `{"screen":"` + ev.Screen + `","visible":true}`}).Code; c == 204 {
		t.Fatal("a page with no key said a screen was in sight")
	}

	_, kiosk, _ := e.store.Add("Kitchen", devices.KindKiosk, nil, "", "")
	first, wall := listenAs(t, e, "/events", kiosk)
	if strings.Contains(first, `"screen"`) || bus.ScreensOpen() != 1 {
		t.Fatalf("a wall screen here counted: %q, %d open", first, bus.ScreensOpen())
	}
	first, shown := listenAs(t, e, "/events", master)
	if !strings.Contains(first, `"screen"`) || bus.ScreensInSight() != 1 {
		t.Fatalf("a screen in sight: %q, %d in sight", first, bus.ScreensInSight())
	}
	shown()
	wall()
	hidden()
	if bus.ScreensOpen() != 0 {
		t.Fatalf("after closing: %d open", bus.ScreensOpen())
	}
	if c := seen(ev.Screen, true); c != 404 {
		t.Fatalf("a closed screen: %d", c)
	}
}
