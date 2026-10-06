package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// elevenLabs is a natural-sounding cloud voice, used when ELEVENLABS_API_KEY
// (or channels.voice.elevenlabs_api_key) is set. Text goes to ElevenLabs;
// nothing else does.
type elevenLabs struct {
	apiKey  string
	voice   string // name or voice id
	model   string
	base    string // https://api.elevenlabs.io
	client  *http.Client
	mu      sync.Mutex
	voiceID string
}

func newElevenLabs(apiKey, voice, model string) *elevenLabs {
	if voice == "" {
		voice = "Daniel"
	}
	if model == "" {
		model = "eleven_flash_v2_5"
	}
	// A sentence that takes longer than this is better said by the backup voice.
	return &elevenLabs{apiKey: apiKey, voice: voice, model: model, base: "https://api.elevenlabs.io", client: &http.Client{Timeout: 20 * time.Second}}
}

// resolve turns a voice name into an id via /v1/voices (ids pass through).
func (e *elevenLabs) resolve(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.voiceID != "" {
		return e.voiceID, nil
	}
	if len(e.voice) >= 20 && !strings.ContainsAny(e.voice, " ") && strings.ToLower(e.voice) != e.voice {
		e.voiceID = e.voice // looks like an id
		return e.voiceID, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.base+"/v1/voices", nil)
	req.Header.Set("xi-api-key", e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("elevenlabs voices: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body struct {
		Voices []struct {
			ID   string `json:"voice_id"`
			Name string `json:"name"`
		} `json:"voices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	for _, v := range body.Voices {
		if strings.EqualFold(v.Name, e.voice) {
			e.voiceID = v.ID
			return v.ID, nil
		}
	}
	return "", fmt.Errorf("elevenlabs: no voice named %q in your library", e.voice)
}

// synthesize returns MP3 bytes.
func (e *elevenLabs) synthesize(ctx context.Context, text string) ([]byte, error) {
	id, err := e.resolve(ctx)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{
		"text":     text,
		"model_id": e.model,
		"voice_settings": map[string]any{
			"stability":         0.45,
			"similarity_boost":  0.8,
			"style":             0.2,
			"use_speaker_boost": true,
		},
	})
	url := fmt.Sprintf("%s/v1/text-to-speech/%s?output_format=mp3_44100_128", e.base, id)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("xi-api-key", e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("elevenlabs tts: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(resp.Body)
}

// play sends an audio file to the platform player. onStart receives the
// process so a caller can pause or stop it.
func play(ctx context.Context, path string, onStart func(*exec.Cmd)) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "afplay", path)
	case "windows":
		ps := fmt.Sprintf(`Add-Type -AssemblyName PresentationCore; $p = New-Object System.Windows.Media.MediaPlayer; $p.Open([uri]'%s'); $p.Play(); Start-Sleep -Milliseconds 500; while ($p.Position -lt $p.NaturalDuration.TimeSpan) { Start-Sleep -Milliseconds 100 }`, path)
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", ps)
	default:
		for _, p := range []string{"mpg123", "ffplay", "mpv"} {
			if _, err := exec.LookPath(p); err == nil {
				if p == "ffplay" {
					cmd = exec.CommandContext(ctx, p, "-nodisp", "-autoexit", "-loglevel", "quiet", path)
				} else if p == "mpv" {
					cmd = exec.CommandContext(ctx, p, "--no-video", "--really-quiet", path)
				} else {
					cmd = exec.CommandContext(ctx, p, "-q", path)
				}
				break
			}
		}
		if cmd == nil {
			return errors.New("no audio player found (install mpg123, ffplay or mpv)")
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if onStart != nil {
		onStart(cmd)
	}
	return cmd.Wait()
}

// synthesizeTo writes MP3 audio for text to path.
func (e *elevenLabs) synthesizeTo(ctx context.Context, text, path string) error {
	audio, err := e.synthesize(ctx, text)
	if err != nil {
		return err
	}
	return os.WriteFile(path, audio, 0o600)
}

// bestMacVoice prefers an installed Enhanced or Premium variant of the voice.
func bestMacVoice(name string) string {
	if name == "" {
		name = "Daniel"
	}
	out, err := exec.Command("say", "-v", "?").Output()
	if err != nil {
		return name
	}
	best := name
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Voice names may contain spaces: "Daniel (Enhanced)  en_GB  # ...".
		full := strings.TrimSpace(strings.SplitN(line, "  ", 2)[0])
		if !strings.HasPrefix(strings.ToLower(full), strings.ToLower(name)) {
			continue
		}
		if strings.Contains(full, "(Premium)") {
			return full
		}
		if strings.Contains(full, "(Enhanced)") {
			best = full
		}
	}
	return best
}
