//go:build darwin

package imessage

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/procenv"
	"github.com/MavrkAI/Mirrin/internal/transcribe"
)

func TestMediaCAFThroughTranscription(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	root := t.TempDir()
	wav := make([]byte, 44+3200)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], 3200)
	source, caf := filepath.Join(root, "source.wav"), filepath.Join(root, "note.caf")
	if err := os.WriteFile(source, wav, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/usr/bin/afconvert", "-f", "caff", "-d", "LEI16", source, caf)
	cmd.Env = procenv.Base()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CAF fixture: %v %s", err, out)
	}
	a := (row{Audio: true, Attachments: true, Mime: "audio/x-caf", Filename: caf}).attachmentAt(root)
	b, err := a.Fetch(context.Background(), 1<<20)
	if err != nil || a.Mime != "audio/wav" {
		t.Fatal(err, a.Mime)
	}
	bin, model := filepath.Join(root, "whisper-cli"), filepath.Join(root, "model")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'a voice note'\n"), 0700)
	os.WriteFile(model, []byte("model"), 0600)
	// This is the daemon's transcription entry point. No converter is available:
	// success proves Fetch produced the WAV format its fast path understands.
	words, err := transcribe.Transcribe(t.Context(), b, a.Mime, transcribe.Options{Server: "off", Bin: bin, Model: model, TempDir: root, FFmpeg: filepath.Join(root, "no-ffmpeg"), Sox: filepath.Join(root, "no-sox")})
	if err != nil || words != "a voice note" {
		t.Fatalf("transcribe CAF: %q %v", words, err)
	}
}

func TestMediaTIFFThroughImagePipeline(t *testing.T) {
	root := t.TempDir()
	var pngData bytes.Buffer
	png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	source, tiff := filepath.Join(root, "source.png"), filepath.Join(root, "photo.tiff")
	os.WriteFile(source, pngData.Bytes(), 0600)
	cmd := exec.CommandContext(t.Context(), "/usr/bin/sips", "-s", "format", "tiff", source, "--out", tiff)
	cmd.Env = procenv.Base()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TIFF fixture: %v %s", err, out)
	}
	a := (row{Attachments: true, Mime: "image/tiff", Filename: tiff}).attachmentAt(root)
	b, err := a.Fetch(t.Context(), 1<<20)
	if err != nil || a.Mime != "image/jpeg" {
		t.Fatal(err, a.Mime)
	}
	if _, _, err := channels.FitImage(b, channels.PhotoEdge, channels.PhotoBytes); err != nil {
		t.Fatal(err)
	}
}
