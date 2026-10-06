package browser

import "strings"

// chromeVersion is what a started Chrome says about itself.
type chromeVersion struct {
	product   string // "Chrome/153.0.8010.53": the real version, whatever the flags
	userAgent string // the user agent it sends (the --user-agent flag, if set)
}

// learnedUA is the user agent headless Chrome presents: its own, with the
// "HeadlessChrome" that gives it away replaced by "Chrome". It is learned from
// the installed browser, so the version and platform are always the real ones
// and match the client hints Chrome sends alongside; a visible window keeps
// Chrome's own user agent untouched.
type learnedUA struct {
	product string
	ua      string // "" when Chrome's own needs no change
	known   bool
}

// headlessUA is Chrome's own headless user agent without the giveaway, or ""
// when there is nothing to change.
func headlessUA(own string) string {
	if !strings.Contains(own, "HeadlessChrome/") {
		return ""
	}
	return strings.Replace(own, "HeadlessChrome/", "Chrome/", 1)
}

// knownUA is the --user-agent flag for the next headless start, "" until one
// has been learned.
func (s *Session) knownUA() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ua.ua
}

// nextUA decides the flag after a headless start with flag: the same flag
// means keep this Chrome; anything else means start again with that.
func (s *Session) nextUA(flag string, v chromeVersion) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if flag == "" {
		// Started as itself: this is Chrome's own user agent.
		s.ua = learnedUA{product: v.product, ua: headlessUA(v.userAgent), known: true}
		return s.ua.ua
	}
	if s.ua.known && s.ua.product == v.product {
		return flag
	}
	s.ua = learnedUA{} // Chrome was updated since: learn it again
	return ""
}
