package config

import (
	"strings"
	"testing"
)

// reach.step_up can ask for more than the default, never less: there is no
// "off" for dangerous approvals from other devices.
func TestStepUpCantBeTurnedOff(t *testing.T) {
	for _, v := range []string{"", "dangerous", "write", "all", " All "} {
		c := Default()
		c.Reach.StepUp = v
		if err := c.Validate(); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"off", "none", "read", "false"} {
		c := Default()
		c.Reach.StepUp = v
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "reach.step_up") {
			t.Errorf("%q accepted: %v", v, err)
		}
	}
}

// user.quiet_hours is HH:MM-HH:MM or off; anything else is a mistake said
// at once, not quiet hours that never come.
func TestQuietHoursAreReadOrRefused(t *testing.T) {
	for _, v := range []string{"", "off", "OFF", "22:00-07:00", "21:30 - 06:00", "13:00-15:00"} {
		c := Default()
		c.User.QuietHours = v
		if err := c.Validate(); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"10pm-7am", "22:00", "25:00-07:00", "never"} {
		c := Default()
		c.User.QuietHours = v
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "user.quiet_hours") {
			t.Errorf("%q accepted: %v", v, err)
		}
	}
}
