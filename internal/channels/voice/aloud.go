package voice

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Anyone in earshot can say "yes": a guest, the television, a recording.
// So a dangerous action (running a command, paying, deleting) is never
// approved by voice. The yes is refused kindly, and the request goes where
// only the owner can answer it: their phone, and the presence screen. A
// write-level action (sending a message, adding an event) can still be
// approved out loud.

// RefusedAloud is what the twin says when a dangerous action gets a spoken
// yes. sentTo names where the request went ("Telegram"), or is empty when
// only the presence screen has it.
func RefusedAloud(id int64, sentTo string) string {
	// Said out loud, a request is "it", never "number 20".
	_ = id
	if sentTo != "" {
		return fmt.Sprintf("That one's too big to take a yes out loud, since anyone nearby could say it. I've sent it to your %s; approve it there, or on the screen.", sentTo)
	}
	return "That one's too big to take a yes out loud, since anyone nearby could say it. Approve it on the screen."
}

// ApproveByHand is the message sent to the owner's phone for a dangerous
// action someone said yes to out loud.
func ApproveByHand(id int64, what string) string {
	return fmt.Sprintf("Someone said yes out loud to #%d (%s). Big steps need your own yes: reply \"yes %d\" to go ahead, or \"no %d\" to drop it.", id, what, id, id)
}

// AskByHand replaces the model's spoken question when a new dangerous
// approval is waiting. It does not claim a phone delivery that hasn't happened.
func AskByHand(id int64, what string) string {
	_ = id
	return fmt.Sprintf("I need your OK: %s. Approve it on the screen, or say yes and I'll send it to your phone.", LowerFirst(strings.TrimRight(what, ". ")))
}

// LowerFirst lowercases a sentence's first letter to continue another
// ("Pay 349.30 AUD…" → "I need your OK to pay 349.30 AUD…"), leaving an
// acronym ("AUD", "MEL") or a name in capitals alone.
func LowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 || !unicode.IsUpper(r) {
		return s
	}
	if r2, _ := utf8.DecodeRuneInString(s[n:]); unicode.IsUpper(r2) {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}
