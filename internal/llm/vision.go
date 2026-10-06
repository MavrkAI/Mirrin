package llm

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Seer is a provider that knows whether its model can look at pictures.
type Seer interface {
	SeesImages(ctx context.Context) bool
}

// SeesImages reports whether p's model can look at a photo. A provider that
// can't say is taken not to, so a photo is answered honestly instead of
// failing the whole turn.
func SeesImages(ctx context.Context, p Provider) bool {
	s, ok := p.(Seer)
	return ok && s.SeesImages(ctx)
}

// SeesImages implements Seer: every Claude model the twin runs on can.
func (a *Anthropic) SeesImages(context.Context) bool { return true }

// SeesImages implements Seer. Ollama is asked what the model can do; for
// the hosted providers and other servers the model's name decides.
func (o *OpenAICompat) SeesImages(ctx context.Context) bool {
	switch o.name {
	case "gemini":
		return true
	case "openai":
		return !reOpenAIText.MatchString(strings.ToLower(o.model))
	case "ollama":
		if sees, ok := o.ollamaVision(ctx); ok {
			return sees
		}
	}
	return reVisionName.MatchString(strings.ToLower(o.model))
}

// reOpenAIText matches OpenAI's text-only chat models; the rest take images.
var reOpenAIText = regexp.MustCompile(`^(gpt-3\.5|gpt-4(-0\d{3}|-32k)?$|o1-mini|o3-mini|gpt-oss|text-|davinci|babbage)`)

// reVisionName matches the names of models known to take images, on Ollama
// (when it can't be asked) and on other OpenAI-compatible servers.
var reVisionName = regexp.MustCompile(`llava|vision|-vl\b|vl:|qwen[0-9.]*-?vl|gemma-?3|gemma3|llama-?4|minicpm-v|moondream|pixtral|mistral-small-?3\.[12]|granite3\.2-vision|claude|gpt-4o|gpt-4\.1|gpt-5|gemini`)

// ollamaVision asks Ollama whether the model lists the vision capability.
// The answer is remembered per server and model; ok is false when Ollama
// couldn't be asked or doesn't say.
func (o *OpenAICompat) ollamaVision(ctx context.Context) (sees, ok bool) {
	key := o.baseURL + "|" + o.model
	visionMu.Lock()
	v, known := visionSeen[key]
	visionMu.Unlock()
	if known {
		return v, true
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	root := &OpenAICompat{name: o.name, baseURL: strings.TrimSuffix(o.baseURL, "/v1"), apiKey: o.apiKey, http: o.http}
	if err := root.do(ctx, http.MethodPost, "/api/show", map[string]string{"model": o.model}, &show); err != nil || show.Capabilities == nil {
		return false, false
	}
	sees = slices.Contains(show.Capabilities, "vision")
	visionMu.Lock()
	visionSeen[key] = sees
	visionMu.Unlock()
	return sees, true
}

var (
	visionMu   sync.Mutex
	visionSeen = map[string]bool{}
)
