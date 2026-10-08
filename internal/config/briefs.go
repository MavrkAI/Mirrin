package config

// Briefs reports whether the twin may say a line before the owner meets
// someone it knows about (watch.meeting_briefs). Unset is on; false turns
// them off.
func (w Watch) Briefs() bool { return w.MeetingBriefs == nil || *w.MeetingBriefs }
