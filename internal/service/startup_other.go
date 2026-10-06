//go:build !windows

package service

import (
	"errors"
	"io"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// The Startup entry is Windows' background twin; elsewhere launchd and
// systemd run it, and these are never called.

func controlStartup(*config.Config, string, func() bool, io.Writer) error {
	return errors.New("the Startup folder is Windows only")
}

func stateStartup() (installed, running bool) { return false, false }

func tidyStartup(io.Writer) {}
