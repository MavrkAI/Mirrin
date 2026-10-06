package mailtext

import (
	"regexp"
	"strings"
)

var (
	// "On Mon, 3 Oct 2026 at 10:02, Sarah <sarah@example.com> wrote:" and its
	// common translations. See attribution for what else such a line needs.
	reWrote      = regexp.MustCompile(`(?i)^((on|le|am|el|il|op|em|den|på|w dniu|dne)\b.*\b(wrote|a écrit|schrieb|escribió|ha scritto|schreef|escreveu|skrev|napisał|napsal)\s*:|(am|op|den|på)\b.*\b(schrieb|schreef|skrev)\b.*:)\s*$`)
	reWroteStart = regexp.MustCompile(`(?i)^(on|le|am|el|il|op|em|den|på|w dniu|dne)\s`)
	reWroteEnd   = regexp.MustCompile(`(?i)\b(wrote|a écrit|schrieb|escribió|ha scritto|schreef|escreveu|skrev|napisał|napsal)\s*:\s*$`)
	reOriginal   = regexp.MustCompile(`(?i)^-{2,}\s*(original message|ursprüngliche nachricht|message d'origine|mensaje original|messaggio originale|oorspronkelijk bericht)\s*-{2,}\s*$`)
	reHeaderFrom = regexp.MustCompile(`(?i)^\*?(from|von|de|da|van)\s*:\*?\s+\S`)
	reHeaderMore = regexp.MustCompile(`(?i)^\*?(sent|date|to|subject|gesendet|datum|an|betreff|envoyé|objet|enviado|asunto|inviato|oggetto)\s*:\*?\s`)
	reRule       = regexp.MustCompile(`^_{20,}\s*$`)
	reForwarded  = regexp.MustCompile(`(?i)(forwarded message|begin forwarded message|weitergeleitete nachricht|message transféré|mensaje reenviado|messaggio inoltrato)`)
	reFwdSubject = regexp.MustCompile(`(?i)^\s*(fwd?|wg|tr|rv|i)\s*:`)
	// Sign-offs a phone or a mail app adds under every message.
	reDeviceLine = regexp.MustCompile(`(?i)^(sent from my \w+|sent from (yahoo )?mail for \w+|sent from yahoo mail|get outlook for (ios|android)|sent from outlook for (ios|android)|sent via \w+ mail|envoyé de mon \w+|von meinem \w+ gesendet)\b.*$`)
	// A numbered or bulleted line: what follows a bare "--" is a list, not a signature.
	reListItem = regexp.MustCompile(`^\s*(\d+[.)]|[-*•])\s`)
)

// Clean sets aside what a reader of this message doesn't need: the earlier
// messages quoted under a reply, and the signature. It keeps text written
// between quoted lines (an inline reply), and it keeps a forwarded message,
// which is the point of a forward. quoted and signature count the non-blank
// lines it left out of each, so the reader can be told.
func Clean(text string) (out string, quoted, signature int) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	cut, kind := len(lines), ""
	for i := range lines {
		if !contentBefore(lines[:i]) {
			continue
		}
		if k := replyHeader(lines, i); k != "" {
			cut, kind = i, k
			break
		}
	}
	// Answers written in between the quoted lines are the reply itself.
	if kind == "wrote" && inlineReply(lines[cut:]) {
		cut = len(lines)
	}
	// A quoted block that runs to the end of what is left, with nothing new
	// after it, is history too.
	end := cut
	for end > 0 && (strings.HasPrefix(strings.TrimSpace(lines[end-1]), ">") || strings.TrimSpace(lines[end-1]) == "") {
		end--
	}
	if prev := prevNonBlank(lines, end-1, end); prev >= 0 {
		if first, ok := attributionEndingAt(lines, prev); ok {
			end = first // the "… wrote:" line belongs to the quote under it
		} else if strings.HasSuffix(strings.TrimSpace(lines[prev]), ":") {
			end = cut // "as Sarah wrote:": the writer is quoting it on purpose
		}
	}
	if end < cut && contentBefore(lines[:end]) && hasQuote(lines[end:cut]) {
		cut = end
	}
	quoted = nonBlank(lines[cut:])
	kept := lines[:cut]

	// The signature, and "Sent from my iPhone" and friends, at the very end.
	kept, signature = dropSignature(kept)
	return strings.TrimSpace(strings.Join(kept, "\n")), quoted, signature
}

// dropSignature takes the signature and any phone sign-off off the end.
func dropSignature(kept []string) ([]string, int) {
	n := 0
	for i := len(kept) - 1; i > 0; i-- {
		if signatureAt(kept, i) && contentBefore(kept[:i]) {
			n += nonBlank(kept[i+1:])
			kept = kept[:i]
			break
		}
	}
	for len(kept) > 0 {
		t := strings.TrimSpace(kept[len(kept)-1])
		if t == "" {
			kept = kept[:len(kept)-1]
			continue
		}
		if reDeviceLine.MatchString(t) && contentBefore(kept[:len(kept)-1]) {
			kept = kept[:len(kept)-1]
			n++
			continue
		}
		break
	}
	return kept, n
}

// signatureAt reports whether lines[i:] is a signature. Under "-- ", the line
// mail programs put above one, that is anything short enough to be one.
// Under a bare "--", which people also use as a dash, only a few short lines
// that are neither a list nor sentences.
func signatureAt(lines []string, i int) bool {
	l := lines[i]
	if strings.HasPrefix(l, "-- ") && strings.TrimSpace(l) == "--" {
		return len(lines)-i <= 15
	}
	if strings.TrimSpace(l) != "--" {
		return false
	}
	rest := 0
	for _, r := range lines[i+1:] {
		t := strings.TrimSpace(r)
		if t == "" {
			continue
		}
		rest++
		if rest > 6 || len([]rune(t)) > 60 || reListItem.MatchString(t) || sentence(t) {
			return false
		}
	}
	return true
}

// sentence reports whether a line reads as writing rather than a name, a
// title or a phone number.
func sentence(t string) bool {
	return strings.ContainsAny(t[len(t)-1:], ".!?") && len(strings.Fields(t)) >= 5
}

// inlineReply reports whether tail, from a "… wrote:" line on, has new
// writing after its first quoted line: answers typed in between the quoted
// questions. A signature or a phone's sign-off under the quote doesn't count.
func inlineReply(tail []string) bool {
	tail, _ = dropSignature(tail)
	first := -1
	for j, l := range tail {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			first = j
			break
		}
	}
	return first >= 0 && contentBefore(tail[first+1:])
}

// attribution reports whether t is a "… wrote:" line: one of the forms mail
// programs write, ending in a colon, and naming a date, a time or an address.
// "On reflection, here's what I wrote:" is a sentence, not one of those.
func attribution(t string) bool {
	return reWrote.MatchString(t) && strings.HasSuffix(t, ":") && strings.ContainsAny(t, "0123456789@<")
}

// attributionAt reports whether an attribution starts at line i, on one line
// or wrapped onto the next: "On Mon, 3 Oct 2026, Sarah Smith <" /
// "sarah@x.com> wrote:".
func attributionAt(lines []string, i int) bool {
	t := strings.TrimSpace(lines[i])
	if attribution(t) {
		return true
	}
	if !reWroteStart.MatchString(t) || reWroteEnd.MatchString(t) || len(t) >= 200 {
		return false
	}
	next := nextNonBlank(lines, i+1, 2)
	return next >= 0 && attribution(t+" "+strings.TrimSpace(lines[next]))
}

// attributionEndingAt finds an attribution whose last line is line i, and
// returns its first line.
func attributionEndingAt(lines []string, i int) (int, bool) {
	if attribution(strings.TrimSpace(lines[i])) {
		return i, true
	}
	if prev := prevNonBlank(lines, i-1, 2); prev >= 0 && attributionAt(lines, prev) && !attribution(strings.TrimSpace(lines[prev])) {
		return prev, true
	}
	return 0, false
}

// replyHeader reports whether line i starts a quoted earlier message, and
// what kind: "wrote" for an attribution over quoted lines, "header" for a
// separator or a block of From:/Sent:/To: lines over the old message.
func replyHeader(lines []string, i int) string {
	t := strings.TrimSpace(lines[i])
	switch {
	case t == "":
		return ""
	case attributionAt(lines, i):
		return "wrote"
	case reOriginal.MatchString(t):
		return "header"
	case reRule.MatchString(t):
		if next := nextNonBlank(lines, i+1, 2); next >= 0 && reHeaderFrom.MatchString(strings.TrimSpace(lines[next])) {
			return "header"
		}
	case reHeaderFrom.MatchString(t):
		// Outlook's block: From: … then Sent:/Date:, To:, Subject: right under
		// it. Under a "Forwarded message" line it is the forward itself.
		if prev := prevNonBlank(lines, i-1, 2); prev >= 0 && reForwarded.MatchString(lines[prev]) {
			return ""
		}
		more := 0
		for j := i + 1; j < len(lines) && j <= i+6; j++ {
			if reHeaderMore.MatchString(strings.TrimSpace(lines[j])) {
				more++
			}
		}
		if more >= 2 {
			return "header"
		}
	}
	return ""
}

func nonBlank(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

func nextNonBlank(lines []string, from, within int) int {
	for j := from; j < len(lines) && j < from+within; j++ {
		if strings.TrimSpace(lines[j]) != "" {
			return j
		}
	}
	return -1
}

func prevNonBlank(lines []string, from, within int) int {
	for j := from; j >= 0 && j > from-within; j-- {
		if strings.TrimSpace(lines[j]) != "" {
			return j
		}
	}
	return -1
}

// IsForward reports whether a subject marks a forwarded message, whose
// quoted part is the point of it and is kept whole.
func IsForward(subject string) bool { return reFwdSubject.MatchString(subject) }

// contentBefore reports whether any line is new writing (not blank, not quoted).
func contentBefore(lines []string) bool {
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, ">") {
			return true
		}
	}
	return false
}

func hasQuote(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			return true
		}
	}
	return false
}
