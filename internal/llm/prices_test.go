package llm

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestPriceLookup(t *testing.T) {
	b := NewPriceBook(map[string]Price{"custom/my-model": {Input: 1, Output: 2}, "Claude-Opus-5": {Input: 9, Output: 9}})
	cases := []struct {
		model  string
		source PriceSource
		ok     bool
		input  float64
	}{
		{"anthropic/claude-sonnet-5", PriceBuiltIn, true, 2},
		{"anthropic/claude-opus-5", PriceConfig, true, 9}, // the owner's price wins, whatever the case
		{"anthropic/claude-opus-5-5", PriceBuiltIn, true, 4},
		{"openai/gpt-4.1-2025-04-14", PriceBuiltIn, true, 2}, // a dated snapshot
		{"openai/gpt-4.1-mini", PriceBuiltIn, true, 0.4},
		{"gemini/models/gemini-2.5-pro", PriceBuiltIn, true, 1.25},
		{"ollama/llama3.1", PriceLocal, true, 0},
		{"custom/my-model", PriceConfig, true, 1},
		{"custom/other", "", false, 0},
		{"anthropic/claude-opus-5-6", "", false, 0}, // a newer model is not guessed from an older one
	}
	for _, c := range cases {
		p, src, ok := b.Lookup(c.model)
		if ok != c.ok || src != c.source || p.Input != c.input {
			t.Errorf("%s: got %+v %q %v, want input %v from %q (%v)", c.model, p, src, ok, c.input, c.source, c.ok)
		}
	}
}

func TestPriceCost(t *testing.T) {
	p, _ := BuiltinPrice("claude-opus-5")
	got := p.Cost(Tokens{Input: 1_000_000, Output: 100_000, CacheRead: 2_000_000, CacheWrite: 400_000})
	want := 5 + 2.5 + 1 + 2.5
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost %v, want %v", got, want)
	}
	// Without cache prices, reads are a tenth and writes 1.25x of input.
	own := Price{Input: 2, Output: 8}
	if got := own.Cost(Tokens{CacheRead: 1_000_000, CacheWrite: 1_000_000}); math.Abs(got-(0.2+2.5)) > 1e-9 {
		t.Fatalf("default cache pricing: %v", got)
	}
}

// Cache writes cost more than plain input, so they must reach the meter.
func TestAnthropicReportsCacheWrites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":30,"output_tokens":2,"cache_read_input_tokens":500,"cache_creation_input_tokens":1200}}`)
	}))
	defer srv.Close()
	a := &Anthropic{client: anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("k"), option.WithMaxRetries(0)), model: "claude-opus-5"}
	resp, err := a.Complete(context.Background(), Request{Messages: []Message{Text(RoleUser, "hi")}, MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if resp.InputTokens != 30 || resp.OutputTokens != 2 || resp.CacheRead != 500 || resp.CacheWrite != 1200 {
		t.Fatalf("usage not carried through: %+v", resp)
	}
}
