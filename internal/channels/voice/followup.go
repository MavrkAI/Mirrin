package voice

import (
	"fmt"
	"time"
)

// DefaultFollowupSeconds is how long the twin listens for a reply after it
// has spoken, unless channels.voice.followup_seconds says otherwise.
const DefaultFollowupSeconds = 8

// answerSeconds is the shortest follow-up window after the twin has asked
// the owner something: an answer takes a moment's thought, and six seconds
// lost the owner's reply to "Want me to open it in the browser instead?".
const answerSeconds = 12

// followupSeconds is how long the next follow-up window stays open: longer
// when the twin's last words asked something or an approval is waiting.
func (c *Channel) followupSeconds() int {
	n := c.cfg.FollowupSeconds
	if c.expectingAnswer() && n < answerSeconds {
		n = answerSeconds
	}
	return n
}

// listenCommand asks the wake helper to open a follow-up window. A window
// of the configured length is a plain "listen" (the helper already knows
// it); a longer one names its length, as "listen 12".
func (c *Channel) listenCommand(seconds int) string {
	if seconds == c.cfg.FollowupSeconds {
		return "listen"
	}
	return fmt.Sprintf("listen %d", seconds)
}

// followupWait is how long the wake loop waits on a follow-up window before
// giving up on the helper: the window itself, then the longest utterance
// that could start at its last moment, then time to transcribe it.
func (c *Channel) followupWait(seconds int) time.Duration {
	return time.Duration(seconds+c.cfg.MaxSeconds+5) * time.Second
}
