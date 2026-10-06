package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// server posts the WAV to a running whisper.cpp server's /inference.
type server struct{ url string }

// client is for the whisper server, which is on this computer. It never
// follows a redirect, so audio can't be sent on anywhere else.
var client = &http.Client{
	Timeout:       10 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (s server) transcribe(ctx context.Context, wav string, o Options) (string, error) {
	return Post(ctx, s.url, wav, o)
}

// AnswerError is a whisper server's own answer that it couldn't hear a clip
// ({"error": …}): the server works, that clip didn't.
type AnswerError struct{ Msg string }

func (e *AnswerError) Error() string { return "whisper server: " + e.Msg }

// Post sends a 16 kHz mono WAV to the whisper.cpp server whose routes start
// at base (its /inference) and returns the text it heard, uncleaned. The
// audio never follows a redirect. An error is an *AnswerError when the
// server answered but couldn't hear this clip.
func Post(ctx context.Context, base, wav string, o Options) (string, error) {
	audio, err := os.ReadFile(wav)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "voice.wav")
	if err != nil {
		return "", err
	}
	_, _ = fw.Write(audio)
	_ = mw.WriteField("response_format", "json")
	_ = mw.WriteField("temperature", "0.0")
	_ = mw.WriteField("temperature_inc", "0.2") // retry a garbled clip a little warmer
	// Without a language the server assumes English; "auto" detects it (a
	// server running an English-only model keeps to English).
	_ = mw.WriteField("language", or(o.Language, "auto"))
	if o.Prompt != "" {
		_ = mw.WriteField("prompt", o.Prompt)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/inference", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper server: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &out) != nil {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(resp.Status + " " + lastLine(string(data)))
		}
		return "", fmt.Errorf("whisper server: %s", msg)
	}
	if out.Error != "" {
		return "", &AnswerError{Msg: out.Error}
	}
	return out.Text, nil
}

// serverURL is the server to look for, "" for none.
func serverURL(s string) string {
	switch s = strings.TrimRight(strings.TrimSpace(s), "/"); strings.ToLower(s) {
	case "":
		return DefaultServer
	case "off", "none", "no":
		return ""
	}
	return s
}

// A server's presence is remembered briefly, so a run of voice notes
// doesn't probe it each time, and one started later is still found. One
// that failed to transcribe is left alone for longer.
const (
	probeFor = 30 * time.Second
	asideFor = 10 * time.Minute
)

var (
	probeMu sync.Mutex
	probed  = map[string]probe{}
)

type probe struct {
	up    bool
	until time.Time
}

// serverUp reports whether a whisper.cpp server answers at url. It must say
// it is one, so audio is never posted to some other program that happens
// to use the same port.
func serverUp(ctx context.Context, url string) bool {
	probeMu.Lock()
	p, ok := probed[url]
	probeMu.Unlock()
	if ok && time.Now().Before(p.until) {
		return p.up
	}
	up := looksLikeWhisper(ctx, url)
	probeMu.Lock()
	probed[url] = probe{up: up, until: time.Now().Add(probeFor)}
	probeMu.Unlock()
	return up
}

// setAside stops using the server at url for a while (it failed to
// transcribe); it is looked for again after that.
func setAside(url string) {
	probeMu.Lock()
	probed[url] = probe{up: false, until: time.Now().Add(asideFor)}
	probeMu.Unlock()
}

// looksLikeWhisper reports whether url is a whisper.cpp server: it names
// itself in its Server header, or its home page is the server's own.
func looksLikeWhisper(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return false
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Server")), "whisper.cpp") {
		return true
	}
	p := strings.ToLower(string(page))
	return strings.Contains(p, "<title>whisper.cpp server</title>") && strings.Contains(p, "response_format")
}

// forget drops what is remembered about url.
func forget(url string) {
	probeMu.Lock()
	delete(probed, url)
	probeMu.Unlock()
}
