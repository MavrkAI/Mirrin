//go:build darwin

package imessage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/procenv"
)

// Native tools understand the formats Messages receives from iPhones. They
// only see a capped copy in a private scratch directory, never chat.db paths.
func nativeConvertAttachment(ctx context.Context, data []byte, target string, max int64) ([]byte, error) {
	if max <= 0 || max > 25<<20 {
		max = 25 << 20
	}
	if int64(len(data)) > max {
		return nil, channels.ErrTooBig
	}
	dir, err := os.MkdirTemp("", "mirrin-imessage-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "input"), filepath.Join(dir, "output")
	if err := os.WriteFile(in, data, 0600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch target {
	case "audio/wav":
		cmd = exec.CommandContext(ctx, "/usr/bin/afconvert", "-f", "WAVE", "-d", "LEI16@16000", "-c", "1", in, out)
	case "image/jpeg":
		cmd = exec.CommandContext(ctx, "/usr/bin/sips", "-s", "format", "jpeg", in, "--out", out)
	default:
		return nil, fmt.Errorf("unsupported attachment conversion")
	}
	cmd.Env = procenv.Base()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("couldn't convert the iMessage attachment: %w", err)
	}
	f, err := os.Open(out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return channels.ReadCapped(f, max)
}
