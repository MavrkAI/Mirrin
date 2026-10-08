package memory

import (
	"strings"
	"unicode"
)

// sensitiveStems are the words that put free text (a portrait's line on
// what's new, say) under one of the subjects Sensitive guards: health,
// money, relationships and secrets. A word counts when it starts with a
// stem, so "doctor's" and "therapist" are caught; stems marked whole must
// match the whole word, so "ex" doesn't catch "exercise".
var sensitiveStems = []struct {
	stem  string
	whole bool
}{
	// health
	{"health", false}, {"medic", false}, {"doctor", false}, {"gp", true}, {"hospital", false},
	{"clinic", false}, {"therap", false}, {"diagnos", false}, {"symptom", false}, {"illness", false},
	{"ill", true}, {"sick", false}, {"pain", true}, {"painful", true}, {"pregnan", false}, {"surgery", false},
	{"prescription", false}, {"pills", true}, {"anxi", false}, {"depress", false}, {"mental", false},
	{"injur", false}, {"cancer", false}, {"diet", false}, {"weight", false}, {"drink", false},
	{"drank", true}, {"drunk", false}, {"alcohol", false}, {"booze", false}, {"hangover", false}, {"smok", false},
	{"cigar", false}, {"vape", false}, {"vaping", false}, {"sleep", false}, {"insomnia", false}, {"stress", false},
	{"grie", false}, {"funeral", false}, {"death", false}, {"died", true}, {"die", true}, {"dying", true},
	{"dead", true}, {"gym", true}, {"gyms", true}, {"calori", false}, {"fat", true}, {"slim", false},
	{"pill", true}, {"drug", false}, {"dentist", false}, {"nurse", false}, {"vaccin", false}, {"migraine", false},
	{"headache", false}, {"lonel", false}, {"sad", true}, {"cry", true}, {"crying", true}, {"burnout", false},
	{"burnt-out", false}, {"burned-out", false}, {"exhaust", false},
	// money
	{"money", false}, {"financ", false}, {"salary", false}, {"debt", false}, {"bank", false},
	{"loan", false}, {"mortgage", false}, {"rent", true}, {"budget", false}, {"spend", false},
	{"savings", false}, {"invest", false}, {"tax", true}, {"taxes", true}, {"pay", true},
	{"paid", true}, {"bills", true}, {"income", false}, {"credit", false}, {"broke", true},
	{"crypto", false}, {"bitcoin", false}, {"stock", false}, {"shares", true}, {"shareholder", false},
	{"afford", false}, {"cost", false}, {"price", false}, {"pricey", false}, {"expens", false}, {"cheap", false},
	{"shop", false}, {"buy", false}, {"bought", true}, {"purchas", false}, {"salar", false}, {"wage", false},
	{"pension", false}, {"insur", false}, {"invoice", false}, {"refund", false}, {"bet", true}, {"bets", true},
	{"betting", true}, {"gambl", false}, {"casino", false}, {"lottery", false}, {"cash", false}, {"wealth", false},
	{"rich", true}, {"poor", true}, {"earn", false}, {"pound", true}, {"pounds", true}, {"dollar", false},
	{"euro", true}, {"euros", true}, {"fund", false}, {"sale", true}, {"sales", true}, {"bargain", false},
	// relationships
	{"relationship", false}, {"partner", false}, {"wife", false}, {"husband", false},
	{"girlfriend", false}, {"boyfriend", false}, {"dating", false}, {"date", true}, {"dates", true},
	{"divorce", false}, {"breakup", false}, {"break-up", false}, {"marri", false}, {"ex", true},
	{"affair", false}, {"romance", false}, {"romantic", false}, {"crush", false},
	{"wedding", false}, {"fianc", false}, {"engage", false}, {"honeymoon", false}, {"love", true}, {"loves", true},
	{"loved", true}, {"lover", false}, {"loving", true}, {"sex", false}, {"flirt", false}, {"kiss", false},
	{"valentine", false}, {"spouse", false}, {"family", false}, {"families", false}, {"mum", false}, {"mom", true},
	{"moms", true}, {"mother", false}, {"dad", true}, {"dads", true}, {"daddy", true}, {"father", false},
	{"parent", false}, {"son", true}, {"sons", true}, {"daughter", false}, {"kid", true}, {"kids", true},
	{"child", false}, {"baby", false}, {"babies", false}, {"sister", false}, {"brother", false}, {"sibling", false},
	{"grand", false}, {"nan", true}, {"nana", true}, {"in-law", false}, {"in-laws", false}, {"aunt", false},
	{"uncle", false}, {"cousin", false}, {"single", true}, {"split", true}, {"cheat", false},
	// secrets
	{"secret", false}, {"private", false}, {"confidential", false}, {"password", false},
	{"passcode", false}, {"pin", true}, {"hidden", true}, {"hide", true}, {"hiding", true}, {"lie", true},
	{"lies", true}, {"lying", true}, {"nobody", true},
}

// MentionsSensitive reports whether text touches a subject Sensitive guards
// (health, money, relationships, secrets). It errs on the side of yes:
// anything it catches is kept quiet, never said out of the blue.
func MentionsSensitive(text string) bool {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && r != '-'
	})
	for _, w := range words {
		w = strings.Trim(w, "-")
		if Sensitive(w) {
			return true
		}
		for _, s := range sensitiveStems {
			if (s.whole && w == s.stem) || (!s.whole && strings.HasPrefix(w, s.stem)) {
				return true
			}
		}
	}
	return strings.ContainsAny(text, "£$€")
}
