package approvals

import (
	"strconv"
	"strings"
	"unicode"
)

// Reply is the owner's answer to an approval request.
type Reply struct {
	Approve bool
	// ID is the approval's number, or 0 for a bare "yes" or "no".
	ID int64
}

// ParseReply reads a message that is only a yes or a no, optionally with the
// approval's number: "yes 12", "Yes.", "ok, go ahead", "no thanks", "👍",
// "yes #12 please". names are words that may address the twin ("Mirrin"),
// so "Yes, Mirrin" still counts. Anything more ("yes, but tomorrow") is not
// a decision; it belongs to the conversation.
func ParseReply(text string, names ...string) (Reply, bool) {
	words := splitNumbers(replyWords(text))
	var id int64
	var rest []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "#" && i+1 < len(words) && isNumber(words[i+1]) {
			continue // "# 12"
		}
		if isNumber(strings.TrimPrefix(w, "#")) {
			if id != 0 {
				return Reply{}, false // two numbers: ask which
			}
			n, err := strconv.ParseInt(strings.TrimPrefix(w, "#"), 10, 64)
			if err != nil || n <= 0 {
				return Reply{}, false
			}
			id = n
			if k := len(rest); k > 0 && (rest[k-1] == "to" || rest[k-1] == "for" || rest[k-1] == "number") {
				rest = rest[:k-1] // "yes to 12"
			}
			continue
		}
		rest = append(rest, w)
	}
	ignore := map[string]bool{}
	for _, n := range names {
		for _, w := range replyWords(n) {
			ignore[w] = true
		}
	}
	yes, no := 0, 0
	for i := 0; i < len(rest); {
		if neutral[rest[i]] || ignore[rest[i]] {
			i++
			continue
		}
		n, affirm := matchPhrase(rest[i:])
		if n == 0 {
			return Reply{}, false
		}
		if affirm {
			yes++
		} else {
			no++
		}
		i += n
	}
	if (yes == 0) == (no == 0) || yes+no > 3 {
		return Reply{}, false // nothing said, a mix of yes and no, or a sentence
	}
	return Reply{Approve: yes > 0, ID: id}, true
}

// replyWords lowercases text, folds emoji to words and drops punctuation
// other than '#', keeping "don't" as one word.
func replyWords(text string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == '\'' || r == '’' || r == '️' || (r >= 0x1F3FB && r <= 0x1F3FF):
			// apostrophes join ("don't" is "dont"); variation selectors and skin tones vanish
		case r == '👍' || r == '✅' || r == '👌' || r == '✔':
			b.WriteString(" yes ")
		case r == '👎' || r == '❌' || r == '🚫' || r == '✖':
			b.WriteString(" no ")
		case r == '#' || unicode.IsLetter(r) || unicode.IsDigit(r):
			if r == '#' {
				b.WriteRune(' ')
			}
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}

// splitNumbers parts a decision word from a number typed against it:
// "yes12" is "yes 12", "n4" is "n 4".
func splitNumbers(words []string) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		i := strings.IndexFunc(w, unicode.IsDigit)
		if i > 0 && isNumber(w[i:]) && (affirm[w[:i]] || deny[w[:i]]) {
			out = append(out, w[:i], w[i:])
			continue
		}
		out = append(out, w)
	}
	return out
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// neutral words may pad a decision without changing it: "yes please",
// "ok then", "no thank you".
var neutral = map[string]bool{
	"please": true, "pls": true, "plz": true, "thanks": true, "thank": true, "you": true,
	"thx": true, "ty": true, "sir": true, "maam": true, "mate": true, "then": true, "just": true,
}

// affirm and deny are the phrases that make up a decision, up to three words each.
var affirm = phrases(
	"yes", "y", "yeah", "yea", "yep", "yup", "ya", "sure", "sure thing", "ok", "okay", "alright", "all right",
	"go", "go ahead", "go on", "do it", "do that", "go for it", "send it", "please do", "approve", "approved",
	"confirm", "confirmed", "sounds good", "absolutely", "of course", "definitely",
)

var deny = phrases(
	"no", "n", "nope", "nah", "deny", "denied", "cancel", "cancel it", "cancel that", "reject",
	"rejected", "dont", "dont do it", "dont do that", "do not", "not now", "hold off", "skip it",
	"leave it", "no way", "never mind", "nevermind",
)

func phrases(ps ...string) map[string]bool {
	m := make(map[string]bool, len(ps))
	for _, p := range ps {
		m[p] = true
	}
	return m
}

// matchPhrase finds the longest decision phrase at the start of words and
// reports how many words it covers and whether it approves.
func matchPhrase(words []string) (int, bool) {
	for n := min(3, len(words)); n > 0; n-- {
		p := strings.Join(words[:n], " ")
		if affirm[p] {
			return n, true
		}
		if deny[p] {
			return n, false
		}
	}
	return 0, false
}
