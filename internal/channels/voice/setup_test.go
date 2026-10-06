package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Acknowledgement clips were cached by voice and index only, so a change of
// speed, of the synthesis script or of a phrase never reached an existing
// install.
func TestAckClipsFollowTheirRecipe(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	made := map[string]int{}
	recipe := "kokoro bm_george speed 1.15 script aaa"
	c := &Channel{dataDir: dir, cfg: config.Voice{Voice: "bm_george"}}
	c.ackMakerFn = func() ackMaker {
		return ackMaker{"wav", recipe, func(_ context.Context, phrase, path string) error {
			mu.Lock()
			made[recipe]++
			mu.Unlock()
			return os.WriteFile(path, []byte(recipe+phrase), 0o644)
		}}
	}
	prepare := func() {
		c.prepareAcks(context.Background())
		<-c.acks.done
	}
	prepare()
	all := len(ackPhrases) + len(plainHellos)
	if made[recipe] != all {
		t.Fatalf("made %d clips, want %d", made[recipe], all)
	}
	prepare()
	if made[recipe] != all {
		t.Fatal("clips were remade with nothing changed")
	}
	first := c.ackClip(false)

	// A slower speed (or a new script) is a new recipe: new clips, old ones gone.
	recipe = "kokoro bm_george speed 1.08 script bbb"
	prepare()
	if made[recipe] != all {
		t.Fatalf("a new recipe made %d clips, want %d", made[recipe], all)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatal("a stale clip was kept")
	}
	b, _ := os.ReadFile(c.ackClip(false))
	if !bytes.HasPrefix(b, []byte(recipe)) {
		t.Fatalf("playing a stale clip: %q", b)
	}
	// An older install's clips ("ack-<voice>-<n>.wav") are cleared too.
	legacy := filepath.Join(dir, "ack-system-bm-george-0.wav")
	_ = os.WriteFile(legacy, []byte("old"), 0o644)
	prepare()
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("an old install's clip was kept")
	}
	if !c.ackClipIsHello(c.ackClip(true)) {
		t.Fatal("a hello after a quiet spell should be a hello clip")
	}
}

// ackClipIsHello reports whether a clip is one of the hello clips.
func (c *Channel) ackClipIsHello(path string) bool {
	c.acks.mu.Lock()
	defer c.acks.mu.Unlock()
	for _, p := range c.acks.hello {
		if p == path {
			return true
		}
	}
	return false
}

func TestAckRecipeNamesSpeedAndScript(t *testing.T) {
	k := &kokoro{voice: "bm_george"}
	c := &Channel{kokoro: k}
	m := c.ackMaker()
	if !strings.Contains(m.recipe, fmt.Sprintf("%.2f", ackSpeed)) || !strings.Contains(m.recipe, shortHash(kokoroScript)) {
		t.Fatalf("recipe %q doesn't name the speed and the script", m.recipe)
	}
	if ackClipName("v", "a", "Right.", "wav") == ackClipName("v", "b", "Right.", "wav") {
		t.Fatal("two recipes share a clip")
	}
}

// rangeServer serves body, honouring Range, and counts requests.
func rangeServer(t *testing.T, body []byte) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, r, "model.bin", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &ranges
}

func knownFor(t *testing.T, name string, body []byte) {
	h := sha256.Sum256(body)
	knownFiles[name] = knownFile{int64(len(body)), hex.EncodeToString(h[:])}
	t.Cleanup(func() { delete(knownFiles, name) })
}

// A download used to start from zero every time, show no progress and
// never be checked.
func TestDownloadResumesAndChecksTheFile(t *testing.T) {
	body := bytes.Repeat([]byte("whisper-weights-"), 64<<10) // 1 MiB
	srv, ranges := rangeServer(t, body)
	dir := t.TempDir()
	path := filepath.Join(dir, "ggml-test.bin")
	knownFor(t, "ggml-test.bin", body)
	// An earlier run stopped a third of the way in.
	if err := os.WriteFile(path+".part", body[:len(body)/3], 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := download(context.Background(), srv.URL+"/m", path, &out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, body) {
		t.Fatal("the file isn't what the server has")
	}
	if len(*ranges) != 1 || (*ranges)[0] != "bytes="+strconv.Itoa(len(body)/3)+"-" {
		t.Fatalf("requests %q: it didn't pick up where it stopped", *ranges)
	}
	for _, want := range []string{"Picking up ggml-test.bin", "(100%)", "Downloaded ggml-test.bin"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("progress %q lacks %q", out.String(), want)
		}
	}
	// Run again: kept, since it checks out.
	out.Reset()
	if err := download(context.Background(), srv.URL+"/m", path, &out); err != nil || len(*ranges) != 1 {
		t.Fatalf("a good file was fetched again (%v, %d requests)", err, len(*ranges))
	}
}

func TestDownloadRefusesAFileThatDoesntMatch(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 4096)
	srv, _ := rangeServer(t, body)
	dir := t.TempDir()
	path := filepath.Join(dir, "voices-test.bin")
	knownFor(t, "voices-test.bin", bytes.Repeat([]byte("y"), 4096)) // what was published
	var out bytes.Buffer
	err := download(context.Background(), srv.URL+"/v", path, &out)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a file that failed its check was put in place")
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Fatal("a bad partial file would be resumed next time")
	}
	// A damaged file already in place is replaced.
	good := bytes.Repeat([]byte("y"), 4096)
	srv2, _ := rangeServer(t, good)
	_ = os.WriteFile(path, bytes.Repeat([]byte("z"), 4096), 0o644)
	out.Reset()
	if err := download(context.Background(), srv2.URL+"/v", path, &out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, good) || !strings.Contains(out.String(), "damaged") {
		t.Fatalf("damaged file not replaced; said %q", out.String())
	}
}

func TestPublishedChecksumsCoverEveryDownload(t *testing.T) {
	for _, name := range []string{"ggml-small.en.bin", "ggml-small.bin", "kokoro-v1.0.onnx", "voices-v1.0.bin"} {
		k, ok := knownFiles[name]
		if !ok || k.size <= 0 || len(k.sha256) != 64 {
			t.Errorf("%s has no published checksum", name)
		}
	}
	for _, lang := range []string{"", "en", "fr", "ja"} {
		if _, ok := knownFiles["ggml-"+WhisperModelFor(lang)+".bin"]; !ok {
			t.Errorf("the model for %q has no checksum", lang)
		}
	}
}

// Setup always downloaded the English-only model, so a French speaker's
// twin could never understand them.
func TestHearingModelFollowsTheLanguage(t *testing.T) {
	for lang, want := range map[string]string{"": "small.en", "en": "small.en", "fr": "small", "ja": "small", "pt": "small"} {
		if got := WhisperModelFor(lang); got != want {
			t.Errorf("WhisperModelFor(%q) = %q, want %q", lang, got, want)
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	none := func() string { return "" }
	for _, c := range []struct {
		env  map[string]string
		os   string
		want string
	}{
		{map[string]string{"LANG": "fr_FR.UTF-8"}, "", "fr"},
		{map[string]string{"LC_ALL": "de_DE.UTF-8", "LANG": "en_US.UTF-8"}, "", "de"},
		{map[string]string{"LANG": "C"}, "", ""},
		{map[string]string{"LANG": "en_AU.UTF-8"}, "ja-JP", "ja"}, // the desktop's language wins
		{nil, "pt-BR", "pt"},
		{nil, "zh-Hans-CN", "zh"},
		{nil, "nb-NO", "no"},
		{nil, "", ""},
	} {
		osLang := none
		if c.os != "" {
			v := c.os
			osLang = func() string { return v }
		}
		if got := systemLanguage(env(c.env), osLang); got != c.want {
			t.Errorf("env %v, os %q: got %q, want %q", c.env, c.os, got, c.want)
		}
	}
	if !EnglishOnly("/x/ggml-small.en.bin") || EnglishOnly("/x/ggml-small.bin") {
		t.Fatal("EnglishOnly")
	}
	if LanguageName("fr") != "French" {
		t.Fatal("LanguageName")
	}
}

// The ElevenLabs key was read from the environment only, so a key saved
// with the others had to be exported to every program the twin starts.
func TestElevenLabsKeyComesFromSavedSecrets(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	t.Setenv("ELEVENLABS_API_KEY", "")
	if ElevenLabsKey(config.Voice{}) != "" {
		t.Fatal("no key anywhere")
	}
	if err := config.SaveSecrets(map[string]string{"ELEVENLABS_API_KEY": "el-saved"}); err != nil {
		t.Fatal(err)
	}
	if got := ElevenLabsKey(config.Voice{}); got != "el-saved" {
		t.Fatalf("got %q", got)
	}
	c := New(config.Voice{KokoroDir: t.TempDir()}, "Mirrin", t.TempDir())
	if c.eleven == nil || c.eleven.apiKey != "el-saved" {
		t.Fatal("the voice didn't use the saved key")
	}
	if ElevenLabsKey(config.Voice{ElevenLabsAPIKey: "in-config"}) != "in-config" {
		t.Fatal("the config's own key comes first")
	}
}
