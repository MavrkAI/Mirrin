package backup

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

// The 12 words are a BIP-39 mnemonic: 128 bits of entropy (K) and a 4-bit
// checksum, 11 bits a word, over the official English wordlist.
//
// bip39_english.txt is copied unchanged from github.com/bitcoin/bips
// (bip-0039/english.txt, SHA-256 2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda,
// checked by a test). BIP-39 is published under the MIT licence
// ("License: MIT" in bip-0039.mediawiki); the identical list ships in the
// reference implementation, trezor/python-mnemonic, MIT, Copyright (c)
// 2013-2016 Pavol Rusnak.
//
//go:embed bip39_english.txt
var wordlistText string

var wordlist, wordIndex = loadWordlist()

func loadWordlist() ([]string, map[string]int) {
	words := strings.Fields(wordlistText)
	if len(words) != 2048 {
		panic("backup: the BIP-39 wordlist must have 2048 words")
	}
	idx := make(map[string]int, len(words))
	for i, w := range words {
		idx[w] = i
	}
	return words, idx
}

// PhraseWords is how many words a Recovery Kit has.
const PhraseWords = 12

// Phrase is the 12 words that unlock a twin's backups. It is never written
// anywhere by Mirrin: only the public keys derived from it are kept.
type Phrase struct {
	k [16]byte
}

// NewPhrase makes a new random phrase (128 bits).
func NewPhrase() (Phrase, error) {
	var p Phrase
	if _, err := rand.Read(p.k[:]); err != nil {
		return p, err
	}
	return p, nil
}

// PhraseFromEntropy is the phrase for 16 bytes of entropy (test vectors).
func PhraseFromEntropy(b []byte) (Phrase, error) {
	var p Phrase
	if len(b) != len(p.k) {
		return p, fmt.Errorf("a phrase is %d bytes, not %d", len(p.k), len(b))
	}
	copy(p.k[:], b)
	return p, nil
}

// String hides the words, so a phrase never ends up in a log by accident.
func (p Phrase) String() string { return "(12 backup words, hidden)" }

// GoString hides the words from %#v too.
func (p Phrase) GoString() string { return p.String() }

// Words returns the 12 words in order.
func (p Phrase) Words() []string {
	cs := sha256.Sum256(p.k[:])
	// 132 bits: the entropy, then the first 4 bits of its SHA-256.
	bits := make([]byte, 0, 17)
	bits = append(bits, p.k[:]...)
	bits = append(bits, cs[0]&0xf0)
	out := make([]string, PhraseWords)
	for i := range out {
		idx := 0
		for b := 0; b < 11; b++ {
			pos := i*11 + b
			bit := (bits[pos/8] >> (7 - uint(pos%8))) & 1
			idx = idx<<1 | int(bit)
		}
		out[i] = wordlist[idx]
	}
	return out
}

// Word returns word n, counting from 1.
func (p Phrase) Word(n int) string {
	if n < 1 || n > PhraseWords {
		return ""
	}
	return p.Words()[n-1]
}

// PhraseError says what is wrong with typed words, and how to fix it.
type PhraseError struct {
	// Word is the position (from 1) of a word that isn't on the list, or 0.
	Word int
	// Got is what was typed there; Suggest is the likely word, if there is one.
	Got, Suggest string
	// Count is how many words there were, when it wasn't 12 (Word is 0).
	Count int
	// Checksum is set when every word is on the list but they don't fit together.
	Checksum bool
}

func (e *PhraseError) Error() string {
	switch {
	case e.Checksum:
		return "those words are all real, but they don't fit together. Check each one against your Recovery Kit (two may be swapped)"
	case e.Word > 0 && e.Suggest != "":
		return fmt.Sprintf("word %d (%q) isn't one of the Recovery Kit words. Did you mean %q?", e.Word, e.Got, e.Suggest)
	case e.Word > 0:
		return fmt.Sprintf("word %d (%q) isn't one of the Recovery Kit words; check it against your kit", e.Word, e.Got)
	case e.Count == 0:
		return "no words were typed; type the 12 words from your Recovery Kit"
	default:
		return fmt.Sprintf("that's %d words; your Recovery Kit has %d", e.Count, PhraseWords)
	}
}

// ParsePhrase reads 12 words as typed or pasted: any case, separated by
// spaces, commas or new lines. Numbers copied from the Recovery Kit ("1.",
// "7)") are used as positions, so its two columns can be pasted as they are.
// A word may be shortened to its first four letters or more.
func ParsePhrase(s string) (Phrase, error) {
	var p Phrase
	type slot struct {
		pos  int
		word string
	}
	var slots []slot
	pos := 0
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ',' || r == ';'
	}) {
		trimmed := strings.TrimRight(tok, ".):")
		if n, err := strconv.Atoi(trimmed); err == nil {
			pos = n
			continue
		}
		// "1.legal" or "7)worth": a number stuck to its word.
		if i := strings.IndexAny(tok, ".)"); i > 0 {
			if n, err := strconv.Atoi(tok[:i]); err == nil {
				pos, tok = n, tok[i+1:]
			}
		}
		tok = strings.Trim(tok, `.:"'()`)
		if tok == "" {
			continue
		}
		slots = append(slots, slot{pos, tok})
		pos = 0
	}
	if len(slots) != PhraseWords {
		return p, &PhraseError{Count: len(slots)}
	}
	// Positions, when every word has a different one from 1 to 12.
	ordered := make([]string, PhraseWords)
	byPos := true
	for _, s := range slots {
		if s.pos < 1 || s.pos > PhraseWords || ordered[s.pos-1] != "" {
			byPos = false
			break
		}
		ordered[s.pos-1] = s.word
	}
	if !byPos {
		for i, s := range slots {
			ordered[i] = s.word
		}
	}
	idx := make([]int, PhraseWords)
	for i, w := range ordered {
		n, ok := lookupWord(w)
		if !ok {
			return p, &PhraseError{Word: i + 1, Got: w, Suggest: suggest(w)}
		}
		idx[i] = n
	}
	var bits [17]byte
	for i, n := range idx {
		for b := 0; b < 11; b++ {
			if n&(1<<(10-uint(b))) != 0 {
				pos := i*11 + b
				bits[pos/8] |= 1 << (7 - uint(pos%8))
			}
		}
	}
	copy(p.k[:], bits[:16])
	cs := sha256.Sum256(p.k[:])
	if bits[16]&0xf0 != cs[0]&0xf0 {
		return Phrase{}, &PhraseError{Checksum: true}
	}
	return p, nil
}

// lookupWord finds a word, or the one word a prefix of four or more letters
// starts (BIP-39 words differ in their first four letters).
func lookupWord(w string) (int, bool) {
	if n, ok := wordIndex[w]; ok {
		return n, true
	}
	if len(w) < 4 {
		return 0, false
	}
	found := -1
	for i, cand := range wordlist {
		if strings.HasPrefix(cand, w) {
			if found >= 0 {
				return 0, false
			}
			found = i
		}
	}
	return found, found >= 0
}

// suggest returns the likeliest word for a typo: the one sharing its first
// four letters, else the closest within two edits.
func suggest(w string) string {
	if len(w) >= 4 {
		for _, cand := range wordlist {
			if strings.HasPrefix(cand, w[:4]) {
				return cand
			}
		}
	}
	// Closest within two edits; on a tie, the one nearest in length.
	best, bestD, bestL := "", 3, 0
	for _, cand := range wordlist {
		d := editDistance(w, cand, bestD+1)
		l := abs(len(cand) - len(w))
		if d < bestD || (d == bestD && best != "" && l < bestL) {
			best, bestD, bestL = cand, d, l
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// editDistance is the edit distance (a swap of two neighbouring letters
// counts as one edit), or max once it reaches it.
func editDistance(a, b string, max int) int {
	if d := len(a) - len(b); d >= max || -d >= max {
		return max
	}
	// Three rows of the optimal string alignment table.
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev = prev, cur
	}
	return min(prev[len(b)], max)
}
