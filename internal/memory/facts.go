package memory

import (
	"context"
	"sort"
	"strings"
	"unicode"
)

// PromptFacts picks the facts to put in front of the model for one message.
// While every fact fits in budget (characters of "- id: subject: fact" lines)
// it returns them all. Past that it takes the facts that share words with
// query, best matches first, for up to half the budget, then fills the rest
// with the newest facts, so a correction ("moved to Sydney") always beats the
// fact it replaces. The result is oldest first; total is how many facts are
// stored, so the caller can tell the model when it isn't seeing everything.
func (s *Store) PromptFacts(ctx context.Context, query string, budget int) ([]Fact, int, error) {
	all, err := s.queryFacts(ctx, `SELECT id, subject, content, source, created_at FROM facts ORDER BY id ASC`)
	if err != nil {
		return nil, 0, err
	}
	total := len(all)
	used := 0
	for _, f := range all {
		used += factCost(f)
	}
	if used <= budget {
		return all, total, nil
	}

	picked := map[int64]bool{}
	used = 0
	take := func(f Fact, limit int) bool {
		c := factCost(f)
		if picked[f.ID] || used+c > limit {
			return false
		}
		picked[f.ID] = true
		used += c
		return true
	}
	if terms := keywords(query); len(terms) > 0 {
		for _, f := range ranked(all, terms, len(all)) {
			take(f, budget/2)
		}
	}
	for i := len(all) - 1; i >= 0; i-- {
		take(all[i], budget)
	}
	out := make([]Fact, 0, len(picked))
	for _, f := range all {
		if picked[f.ID] {
			out = append(out, f)
		}
	}
	return out, total, nil
}

// factCost is the size of a fact's line in the prompt.
func factCost(f Fact) int { return len(f.Subject) + len(f.Content) + 12 }

// ranked returns up to limit facts that share words with terms, those
// matching the most terms first and the newest first among equals.
func ranked(facts []Fact, terms []string, limit int) []Fact {
	type scored struct {
		f     Fact
		score int
	}
	var hits []scored
	for _, f := range facts {
		if n := matches(terms, words(f.Subject+" "+f.Content, 1)); n > 0 {
			hits = append(hits, scored{f, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].f.ID > hits[j].f.ID
	})
	out := make([]Fact, 0, min(limit, len(hits)))
	for _, h := range hits[:min(limit, len(hits))] {
		out = append(out, h.f)
	}
	return out
}

// matches counts the query terms found among a fact's words. A term of two
// letters or fewer ("Ed", "NY") must be a whole word; a longer one may be
// part of one ("mari" finds "Maria").
func matches(terms []string, words []string) int {
	n := 0
	for _, t := range terms {
		for _, w := range words {
			if t == w || len(t) > 2 && (related(t, w) || strings.Contains(w, t)) {
				n++
				break
			}
		}
	}
	return n
}

// related reports whether two stems are the same word give or take an
// ending ("allergy" and "allergic", "manager" and "management").
func related(a, b string) bool {
	if a == b {
		return true
	}
	short := min(len(a), len(b))
	cp := 0
	for cp < short && a[cp] == b[cp] {
		cp++
	}
	return cp >= 5 && cp >= short-2
}

// keywords splits a message into the word stems that pick which facts go in
// the prompt: no little words, none of the words system-framed prompts are
// made of ("task", "heartbeat", "today"), nothing under three letters.
func keywords(text string) []string {
	// A system-framed prompt ("[Scheduled task …] do the briefing") is about
	// what follows the frame.
	if strings.HasPrefix(text, "[") {
		if i := strings.Index(text, "]"); i > 0 && strings.TrimSpace(text[i+1:]) != "" {
			text = text[i+1:]
		}
	}
	return words(text, 3, stopWords, framingWords)
}

// searchTerms splits a recall query into word stems. Two-letter words count
// ("Ed", "NY") and so does any word the user might search for; little words
// count only when the query has nothing else.
func searchTerms(query string) []string {
	if t := words(query, 2, stopWords); len(t) > 0 {
		return t
	}
	return words(query, 1)
}

// words splits text into distinct lower-case word stems of at least minLen
// letters, leaving out any found in skip.
func words(text string, minLen int, skip ...map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
next:
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		w = strings.Trim(w, "'")
		w = strings.TrimSuffix(w, "'s")
		if len([]rune(w)) < minLen {
			continue
		}
		for _, sk := range skip {
			if sk[w] {
				continue next
			}
		}
		w = stem(w)
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// stem trims common English endings so "allergies" meets "allergy".
func stem(w string) string {
	for _, s := range []struct{ suffix, repl string }{{"ies", "y"}, {"ing", ""}, {"ed", ""}, {"es", ""}, {"s", ""}} {
		if strings.HasSuffix(w, s.suffix) && len(w)-len(s.suffix)+len(s.repl) >= 3 && !strings.HasSuffix(w, "ss") {
			return strings.TrimSuffix(w, s.suffix) + s.repl
		}
	}
	return w
}

// stopWords are little words that would match everything.
var stopWords = wordSet(`a an am as at be by do if in is it of on or so to up no ok
	the and for are but not you your yours with have has had this that these those what when where who whom why how
	can could would should will shall just about from into onto they them their there then than was were been being our ours out get got
	let may might must also any some all more most very too did does doing done its it's i'm i've i'd i'll me my mine his her hers him she he
	we us which while want need like know tell please thanks thank make made take say said
	something anything nothing everything someone anyone thing things way yes okay sure really still`)

// framingWords fill system-framed prompts ("[Scheduled task from your
// heartbeat …]"). They mislead when choosing facts for the prompt but are
// fair search terms for recall.
var framingWords = wordSet(`now today tonight tomorrow yesterday user user's one two
	reply replied task scheduled system typed heartbeat protocol approved denied executed result outcome lines`)

func wordSet(list string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(list) {
		m[w] = true
	}
	return m
}
