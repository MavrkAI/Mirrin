package config

import "strings"

// FollowsSystem reports whether a timezone setting means "whatever the
// system says": empty, "Local" or "auto", in any case. Anything else names a
// zone the owner pinned.
func FollowsSystem(timezone string) bool {
	s := strings.TrimSpace(timezone)
	return s == "" || strings.EqualFold(s, "local") || strings.EqualFold(s, "auto")
}
