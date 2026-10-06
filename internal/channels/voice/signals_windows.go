//go:build windows

package voice

import (
	"errors"
	"os"
)

// Windows has no SIGSTOP; barge-in stops playback outright instead of pausing.
func pauseProcess(*os.Process) error  { return errors.New("pause not supported") }
func resumeProcess(*os.Process) error { return nil }
