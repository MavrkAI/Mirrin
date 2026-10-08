package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// On this computer, Why? under a reply lists the facts it drew on with a
// plain lead, and once one is forgotten from there it isn't listed again.
func TestWhyListsTheFactsAndForgetTakesOneAway(t *testing.T) {
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	priya, _ := store.Remember(ctx, "family", "Priya is Tony's sister and lives in Pune.", "test")
	acme, _ := store.Remember(ctx, "work", "Tony works at Acme.", "test")
	if err := store.NoteWhy(ctx, memory.WhyNote{ChatKey: "screen:local", Reply: "Priya's in Pune.", Recalled: []int64{priya}, Matched: []int64{acme}}); err != nil {
		t.Fatal(err)
	}
	const master = "0123456789abcdef0123456789abcdef0123456789abcdef"
	srv := api.New("127.0.0.1:7742", master, nil).WithMemory(memoryAdapter{store: store})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = srv.Serve(sctx, ln, api.LoopbackOnly, "loopback"); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	do := func(method, path string, body any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, "http://"+ln.Addr().String()+path, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+master)
		r.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	why := func(reply string) api.Why {
		t.Helper()
		resp := do("POST", "/memory/why", map[string]string{"reply": reply})
		defer resp.Body.Close()
		var w api.Why
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&w) != nil {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return w
	}
	w := why("Priya's in Pune.")
	if !w.Known || len(w.Facts) != 2 || w.Facts[0].ID != priya || w.Facts[0].How != "recalled" || w.Lead != "These are the things you've told me that I drew on:" {
		t.Fatalf("why = %+v", w)
	}
	resp := do("DELETE", fmt.Sprintf("/memory/facts/%d", priya), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forget: status %d", resp.StatusCode)
	}
	if w := why("Priya's in Pune."); len(w.Facts) != 1 || w.Facts[0].ID != acme {
		t.Fatalf("after forgetting, why = %+v", w)
	}
	if w := why("Something from before notes were kept."); w.Known || len(w.Facts) != 0 || w.Lead == "" {
		t.Fatalf("an unknown reply: %+v", w)
	}
}
