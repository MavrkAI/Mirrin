package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The regression: OpenAI's cached input was counted at the full input
// price, so a twin with a warm prompt cache was told it spent more than it
// did.
func TestOpenAICachedInputCostsLess(t *testing.T) {
	cost := func(usage string, stream bool) (Price, Tokens) {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"))
				_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":" + usage + "}\n\ndata: [DONE]\n\n"))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":` + usage + `}`))
		}))
		defer srv.Close()
		p := NewOpenAICompat("openai", srv.URL, "k", "gpt-5")
		req := Request{Messages: []Message{Text(RoleUser, "hi")}}
		var resp *Response
		var err error
		if stream {
			resp, err = p.Stream(context.Background(), req, func(string) {})
		} else {
			resp, err = p.Complete(context.Background(), req)
		}
		if err != nil {
			t.Fatal(err)
		}
		price, _, ok := NewPriceBook(nil).Lookup("openai/gpt-5")
		if !ok {
			t.Fatal("gpt-5 has a built-in price")
		}
		return price, Tokens{Input: resp.InputTokens, Output: resp.OutputTokens, CacheRead: resp.CacheRead}
	}
	for _, stream := range []bool{false, true} {
		price, cold := cost(`{"prompt_tokens":1000,"completion_tokens":10}`, stream)
		_, warm := cost(`{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":800}}`, stream)
		if warm.CacheRead != 800 || warm.Input != 200 || cold.CacheRead != 0 || cold.Input != 1000 {
			t.Fatalf("stream=%v: tokens cold %+v warm %+v", stream, cold, warm)
		}
		if price.Cost(warm) >= price.Cost(cold) {
			t.Fatalf("stream=%v: cached input should cost less: %f vs %f", stream, price.Cost(warm), price.Cost(cold))
		}
	}
	// gpt-4o and gpt-4o-mini bill cached input at half: without their own
	// cache-read price the Input/10 fallback ran estimates low.
	for _, id := range []string{"gpt-4o", "gpt-4o-mini"} {
		if p, _ := BuiltinPrice(id); p.CacheRead != p.Input/2 {
			t.Errorf("%s: cache-read price %v, want %v", id, p.CacheRead, p.Input/2)
		}
	}
	for _, id := range []string{"gpt-5", "gpt-4.1"} {
		if p, _ := BuiltinPrice(id); p.CacheRead == 0 || p.CacheRead >= p.Input {
			t.Errorf("%s: cache-read price %v", id, p.CacheRead)
		}
	}
}
