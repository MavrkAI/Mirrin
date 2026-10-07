package daemon

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// A voice chosen while the twin listens all the time is the one it speaks
// in next, without a restart: the microphone pipeline is rebuilt from the
// new settings. Other settings leave it alone.
func TestChangingVoiceSwitchesWithoutARestart(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	rebuilds := 0
	old := rebuildVoice
	t.Cleanup(func() { rebuildVoice = old })
	rebuildVoice = func(*Daemon) error { rebuilds++; return nil }
	td.channels["voice"] = &fakeChannel{name: "voice", owner: "local", out: make(chan string, 8)} // listening
	if err := td.UpdateConfig(func(c *config.Config) {
		c.Channels.Voice.Enabled, c.Channels.Voice.Mode = true, "wake"
	}); err != nil {
		t.Fatal(err)
	}
	rebuilds = 0

	if err := td.UpdateConfig(func(c *config.Config) {
		c.Channels.Voice.Engine, c.Channels.Voice.Voice = "system", "Daniel"
	}); err != nil {
		t.Fatal(err)
	}
	if rebuilds != 1 {
		t.Fatalf("a new voice rebuilt listening %d times, want 1", rebuilds)
	}
	if v := td.Config().Channels.Voice; v.Voice != "Daniel" || v.Engine != "system" {
		t.Fatalf("live voice %s/%s", v.Engine, v.Voice)
	}

	if err := td.UpdateConfig(func(c *config.Config) { c.LLM.Effort = "low" }); err != nil {
		t.Fatal(err)
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.Channels.Voice.Voice = "Daniel" }); err != nil {
		t.Fatal(err)
	}
	if rebuilds != 1 {
		t.Fatalf("an unchanged voice rebuilt listening (%d rebuilds)", rebuilds)
	}
}
