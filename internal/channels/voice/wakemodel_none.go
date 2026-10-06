//go:build !wakemodel

package voice

// bundledWakeModel is empty: this build carries no wake-word model, so
// Mirrin uses a "Hey Mirrin" model only when one is already in the voice
// folder (wakemodel.go).
var bundledWakeModel []byte
