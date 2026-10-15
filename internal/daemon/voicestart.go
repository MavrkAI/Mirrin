package daemon

// listensOnStart reports whether Run starts always-on listening: the settings
// want it (the default), and this is the twin under the service or the menu
// bar. A conversation in a terminal (`mirrin chat` with no twin running) is
// not the twin's home and never opens the microphone, whatever the settings.
func (d *Daemon) listensOnStart() bool {
	v := d.cfg.Channels.Voice
	return v.Enabled && v.Mode == "wake" && !d.interactive
}
