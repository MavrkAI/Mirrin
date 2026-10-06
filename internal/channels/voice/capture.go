package voice

import (
	"os"
)

// snapshotCapture keeps the helper's reusable WAV from changing while an
// asynchronous transcription reads it. Call before starting the worker.
func (c *Channel) snapshotCapture(path string) (string, func(), error) {
	if c.transcribeFn != nil {
		return path, func() {}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", func() {}, err
	}
	f, err := os.CreateTemp(c.dataDir, "capture-*.wav")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	_, err = f.Write(data)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
	}
	return f.Name(), cleanup, err
}
