package llm

import (
	"regexp"
	"sort"
	"strings"
)

// Price is what a model costs, in US dollars per million tokens. Everything
// computed from it is an estimate: providers change prices, give discounts
// and bill in ways a token count can't see. The provider's bill is what
// counts.
type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	// CacheRead and CacheWrite are prompt-cache reads and writes. Left at
	// zero they are taken as a tenth and 1.25 times the input price, which
	// is how most providers charge.
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Tokens counts what one or more model calls used.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Add returns the sum of two counts.
func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{t.Input + o.Input, t.Output + o.Output, t.CacheRead + o.CacheRead, t.CacheWrite + o.CacheWrite}
}

// Cost is the estimated price of t in US dollars.
func (p Price) Cost(t Tokens) float64 {
	read, write := p.CacheRead, p.CacheWrite
	if read == 0 {
		read = p.Input / 10
	}
	if write == 0 {
		write = p.Input * 1.25
	}
	return (float64(t.Input)*p.Input + float64(t.Output)*p.Output + float64(t.CacheRead)*read + float64(t.CacheWrite)*write) / 1e6
}

// builtinPrices are list prices (US$ per million tokens) for the models the
// twin offers, keyed by model id. They are a starting point, not a promise:
// they go out of date, and anyone can set their own under usage.prices in
// config.yaml. Anthropic's are its published first-party rates; OpenAI's
// count cached input (prompt_tokens_details.cached_tokens) at the cache-read
// price; Gemini's are for prompts up to 200k tokens.
var builtinPrices = map[string]Price{
	// Anthropic
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-mythos-5-1": {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-fable-5":    {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"claude-mythos-5":   {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-7":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-6":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"claude-haiku-4-5":  {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
	// OpenAI
	"gpt-5":        {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5-mini":   {Input: 0.25, Output: 2, CacheRead: 0.025},
	"gpt-5-nano":   {Input: 0.05, Output: 0.4, CacheRead: 0.005},
	"gpt-4.1":      {Input: 2, Output: 8, CacheRead: 0.5},
	"gpt-4.1-mini": {Input: 0.4, Output: 1.6, CacheRead: 0.1},
	"gpt-4.1-nano": {Input: 0.1, Output: 0.4, CacheRead: 0.025},
	"gpt-4o":       {Input: 2.5, Output: 10, CacheRead: 1.25},
	"gpt-4o-mini":  {Input: 0.15, Output: 0.6, CacheRead: 0.075},
	// Google Gemini
	"gemini-2.5-pro":        {Input: 1.25, Output: 10},
	"gemini-2.5-flash":      {Input: 0.3, Output: 2.5},
	"gemini-2.5-flash-lite": {Input: 0.1, Output: 0.4},
}

// PriceSource says where a price came from.
type PriceSource string

const (
	PriceBuiltIn PriceSource = "built in"
	PriceConfig  PriceSource = "your config"
	PriceLocal   PriceSource = "runs on this computer"
)

// PriceBook looks up model prices: the owner's own first, then the built-in
// list. Models run by Ollama cost nothing.
type PriceBook struct {
	own map[string]Price
}

// NewPriceBook makes a price book with the owner's prices (keyed by model id,
// or "provider/model" to be specific), which win over the built-in ones.
func NewPriceBook(own map[string]Price) PriceBook {
	b := PriceBook{own: map[string]Price{}}
	for k, p := range own {
		b.own[strings.ToLower(strings.TrimSpace(k))] = p
	}
	return b
}

// Lookup finds the price for a model as the twin names it ("anthropic/claude-opus-5").
// ok is false when no price is known.
func (b PriceBook) Lookup(model string) (Price, PriceSource, bool) {
	full := strings.ToLower(strings.TrimSpace(model))
	provider, id, found := strings.Cut(full, "/")
	if !found {
		provider, id = "", full
	}
	id = strings.TrimPrefix(id, "models/") // Gemini's long form
	if p, ok := b.own[full]; ok {
		return p, PriceConfig, true
	}
	if p, ok := b.own[id]; ok {
		return p, PriceConfig, true
	}
	if provider == "ollama" {
		return Price{}, PriceLocal, true
	}
	if p, ok := builtinPrices[id]; ok {
		return p, PriceBuiltIn, true
	}
	// A dated snapshot of a listed model ("gpt-4.1-2025-04-14") costs what
	// the model does. Nothing else is guessed: "claude-opus-5-6" is not
	// "claude-opus-5".
	if base := reSnapshot.ReplaceAllString(id, ""); base != id {
		if p, ok := b.own[base]; ok {
			return p, PriceConfig, true
		}
		if p, ok := builtinPrices[base]; ok {
			return p, PriceBuiltIn, true
		}
	}
	return Price{}, "", false
}

// reSnapshot is a snapshot or tag suffix on a model id.
var reSnapshot = regexp.MustCompile(`(-\d{8}|-\d{4}-\d{2}-\d{2}|-latest|@[\w.-]+|:[\w.-]+)$`)

// BuiltinPrices lists the built-in prices, by model id, for showing.
func BuiltinPrices() []string {
	out := make([]string, 0, len(builtinPrices))
	for k := range builtinPrices {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuiltinPrice is the built-in price of a model id.
func BuiltinPrice(id string) (Price, bool) {
	p, ok := builtinPrices[id]
	return p, ok
}
