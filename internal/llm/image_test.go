package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

var jpegBytes = []byte("\xff\xd8\xff\xe0 a photo")

func photoTurn() Message {
	img := ImageBlock("media/2026-09/ab.jpg", "image/jpeg")
	img.Image = jpegBytes
	return Message{Role: RoleUser, Blocks: []Block{img, {Type: BlockText, Text: "(photo) is this mould?"}}}
}

// History keeps where a photo is, never its bytes.
func TestImageBlocksKeepOnlyTheirPath(t *testing.T) {
	b, err := json.Marshal(photoTurn().Blocks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), base64.StdEncoding.EncodeToString(jpegBytes)) || !strings.Contains(string(b), `"path":"media/2026-09/ab.jpg"`) {
		t.Fatalf("stored as %s", b)
	}
	var back []Block
	_ = json.Unmarshal(b, &back)
	if back[0].Type != BlockImage || back[0].Path != "media/2026-09/ab.jpg" || back[0].Image != nil {
		t.Fatalf("read back %+v", back[0])
	}
	if ImageTypeOf(back[0].Path) != "image/jpeg" || ImageTypeOf("x.webp") != "image/webp" || ImageTypeOf("x.heic") != "" {
		t.Fatal("ImageTypeOf")
	}
	if photoTurn().PlainText() != "(photo) is this mould?" {
		t.Fatal("a photo's path must not leak into the text")
	}
}

func TestClaudeSeesThePhoto(t *testing.T) {
	params := toParams([]Message{photoTurn()})
	if len(params) != 1 || len(params[0].Content) != 2 {
		t.Fatalf("params %+v", params)
	}
	img := params[0].Content[0].OfImage
	if img == nil || img.Source.OfBase64 == nil || img.Source.OfBase64.Data != base64.StdEncoding.EncodeToString(jpegBytes) ||
		string(img.Source.OfBase64.MediaType) != "image/jpeg" {
		t.Fatalf("image block %+v", params[0].Content[0])
	}
	// One that wasn't loaded (the agent turned it into a note) is not sent.
	unloaded := Message{Role: RoleUser, Blocks: []Block{ImageBlock("media/x.jpg", "image/jpeg"), {Type: BlockText, Text: "hi"}}}
	if p := toParams([]Message{unloaded}); len(p[0].Content) != 1 || p[0].Content[0].OfText == nil {
		t.Fatalf("unloaded image sent: %+v", p[0].Content)
	}
}

func TestOpenAICompatSendsThePhotoAsAContentPart(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Looks like mould."},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	p := NewOpenAICompat("openai", srv.URL, "k", "gpt-4.1")
	if _, err := p.Complete(context.Background(), Request{Messages: []Message{Text(RoleUser, "hello"), {Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "hi"}}}, photoTurn()}}); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if first := msgs[0].(map[string]any); first["content"] != "hello" {
		t.Fatalf("a plain turn should stay a string: %v", first)
	}
	parts, ok := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("photo turn: %v", msgs[len(msgs)-1])
	}
	img := parts[0].(map[string]any)
	url := img["image_url"].(map[string]any)["url"].(string)
	if img["type"] != "image_url" || url != "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(jpegBytes) {
		t.Fatalf("image part %v", img)
	}
	if txt := parts[1].(map[string]any); txt["type"] != "text" || txt["text"] != "(photo) is this mould?" {
		t.Fatalf("text part %v", txt)
	}
}

type plainProvider struct{}

func (plainProvider) Name() string { return "plain" }
func (plainProvider) Complete(context.Context, Request) (*Response, error) {
	return nil, nil
}

func TestSeesImages(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		p    Provider
		want bool
	}{
		{NewAnthropic("k", "claude-opus-5"), true},
		{NewOpenAICompat("openai", OpenAIBaseURL, "k", "gpt-4.1"), true},
		{NewOpenAICompat("openai", OpenAIBaseURL, "k", "gpt-4o-mini"), true},
		{NewOpenAICompat("openai", OpenAIBaseURL, "k", "gpt-3.5-turbo"), false},
		{NewOpenAICompat("openai", OpenAIBaseURL, "k", "gpt-4"), false},
		{NewOpenAICompat("openai", OpenAIBaseURL, "k", "o3-mini"), false},
		{NewOpenAICompat("gemini", GeminiBaseURL, "k", "gemini-2.5-pro"), true},
		{NewOpenAICompat("custom", "http://localhost:1234/v1", "", "anthropic/claude-sonnet-4"), true},
		{NewOpenAICompat("custom", "http://localhost:1234/v1", "", "some-text-model"), false},
		{plainProvider{}, false},
	} {
		if got := SeesImages(ctx, tt.p); got != tt.want {
			t.Errorf("%s: SeesImages = %v, want %v", tt.p.Name(), got, tt.want)
		}
	}
}

// A model turning a request down over a picture is told apart from every
// other failure, so only then is the picture set aside.
func TestImageRefused(t *testing.T) {
	for msg, want := range map[string]bool{
		`anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content.0.image.source.base64: image exceeds 5 MB maximum"}}`: true,
		`ollama 500: this model is missing data required for image input`:                                                                                           true,
		`openai 400: Invalid content type. image_url is only supported by certain models.`:                                                                          true,
		`anthropic 429: {"type":"error","error":{"type":"rate_limit_error","message":"image rate"}}`:                                                                false,
		`openai 400: Invalid 'messages[1].content': string too long`:                                                                                                false,
		`dial tcp: lookup api.anthropic.com: no such host (image)`:                                                                                                  false,
		`ollama 400: {"error":"registry.ollama.ai/library/llama3.2-vision:latest does not support tools"}`:                                                          false,
		`openai 400: Invalid 'messages[3].content[0].image_url': tool messages can't contain images`:                                                                false,
		`openai 400: You uploaded an unsupported image. Please make sure your image is valid.`:                                                                      true,
	} {
		if got := ImageRefused(errors.New(msg)); got != want {
			t.Errorf("%s: %v", msg, got)
		}
	}
	if ImageRefused(nil) {
		t.Fatal("nil")
	}
	for msg, want := range map[string]string{
		`anthropic 400: {"type":"error","error":{"message":"messages.2.content.0.image.source.base64: image exceeds 5 MB maximum"}}`:           ImageInMessage,
		`anthropic 400: {"type":"error","error":{"message":"messages.4.content.0.content.1.image.source.base64: image exceeds 5 MB maximum"}}`: ImageFromTool,
		`openai 400: Invalid image.`: "",
	} {
		if got := RefusedImageIn(errors.New(msg)); got != want {
			t.Errorf("%s: %q, want %q", msg, got, want)
		}
	}
}

// Ollama is asked, once per model; when it can't be asked the name decides.
func TestOllamaIsAskedWhatItsModelCanSee(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		asked.Add(1)
		var req struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Model {
		case "llama4:scout":
			_, _ = w.Write([]byte(`{"capabilities":["completion","vision","tools"]}`))
		case "llava-renamed":
			_, _ = w.Write([]byte(`{"capabilities":["completion"]}`))
		default:
			_, _ = w.Write([]byte(`{"modelfile":"old ollama says nothing"}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	scout := NewOpenAICompat("ollama", srv.URL+"/v1", "", "llama4:scout")
	first, again := scout.SeesImages(ctx), scout.SeesImages(ctx)
	if !first || !again || asked.Load() != 1 {
		t.Fatalf("llama4:scout: asked %d times", asked.Load())
	}
	if NewOpenAICompat("ollama", srv.URL+"/v1", "", "llava-renamed").SeesImages(ctx) {
		t.Fatal("Ollama's answer beats the name")
	}
	if NewOpenAICompat("ollama", srv.URL+"/v1", "", "gpt-oss:20b").SeesImages(ctx) {
		t.Fatal("a text model with no capabilities listed")
	}
	if !NewOpenAICompat("ollama", "http://127.0.0.1:1/v1", "", "qwen2.5vl:7b").SeesImages(ctx) {
		t.Fatal("Ollama unreachable: the name should decide")
	}
}
