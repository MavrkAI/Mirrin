package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// The memory page reaches past the first 1,000 facts: page 2 is the newest
// remainder, and the total counts every fact.
func TestMemoryPagePastAThousandFacts(t *testing.T) {
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	for i := 1; i <= 1500; i++ {
		if _, err := store.Remember(ctx, "general", fmt.Sprintf("fact number %d", i), "test"); err != nil {
			t.Fatal(err)
		}
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
	r, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/memory/facts?offset=1000&limit=1000", nil)
	r.Header.Set("Authorization", "Bearer "+master)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page api.FactsPage
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&page) != nil {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if page.Total != 1500 || len(page.Facts) != 500 || page.Facts[0].Content != "fact number 1001" || page.Facts[499].Content != "fact number 1500" {
		t.Fatalf("page 2: %d facts of %d", len(page.Facts), page.Total)
	}
}
