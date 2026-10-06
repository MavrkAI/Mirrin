package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"filippo.io/age"
)

// vectors is testdata/vectors.json: the official BIP-39 English 128-bit
// vectors (trezor/python-mnemonic vectors.json) and Mirrin's own golden
// derivations, which docs/backup-format.md repeats.
type vectors struct {
	BIP39 []struct {
		Entropy  string `json:"entropy"`
		Mnemonic string `json:"mnemonic"`
	} `json:"bip39"`
	Mirrin []struct {
		Phrase          string `json:"phrase"`
		Entropy         string `json:"entropy"`
		Recipient       string `json:"recipient_sha256"`
		RecipientPrefix string `json:"recipient_prefix"`
		X25519Recipient string `json:"x25519_recipient"`
		RecoveryPub     string `json:"recovery_pub"`
		Namespace       string `json:"namespace"`
		PQSeed          string `json:"pq_seed"`
		X25519Seed      string `json:"x25519_seed"`
		RecoverySeed    string `json:"recovery_seed"`
	} `json:"antbot"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.BIP39) == 0 || len(v.Mirrin) == 0 {
		t.Fatal("vectors.json is missing vectors")
	}
	return v
}

func TestWordlistIsTheOfficialOne(t *testing.T) {
	sum := sha256.Sum256([]byte(wordlistText))
	if got := hex.EncodeToString(sum[:]); got != "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda" {
		t.Fatalf("bip39_english.txt changed: sha256 %s", got)
	}
}

func TestBIP39VectorsRoundTrip(t *testing.T) {
	for _, v := range loadVectors(t).BIP39 {
		ent, _ := hex.DecodeString(v.Entropy)
		p, err := PhraseFromEntropy(ent)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(p.Words(), " "); got != v.Mnemonic {
			t.Fatalf("%s: got %q, want %q", v.Entropy, got, v.Mnemonic)
		}
		back, err := ParsePhrase(v.Mnemonic)
		if err != nil {
			t.Fatalf("%q: %v", v.Mnemonic, err)
		}
		if back != p {
			t.Fatalf("%q parsed to other entropy", v.Mnemonic)
		}
	}
}

func TestParsePhraseIsForgiving(t *testing.T) {
	want, _ := ParsePhrase("legal winner thank year wave sausage worth useful legal winner thank yellow")
	for _, typed := range []string{
		"  Legal WINNER thank, year wave sausage\nworth useful legal winner thank yellow ",
		// Pasted from the kit's two columns.
		"1. legal     7. worth\n2. winner    8. useful\n3. thank     9. legal\n4. year     10. winner\n5. wave     11. thank\n6. sausage  12. yellow",
		// First four letters are enough.
		"lega winn than year wave saus wort usef lega winn than yell",
	} {
		got, err := ParsePhrase(typed)
		if err != nil || got != want {
			t.Fatalf("%q: %v", typed, err)
		}
	}
}

func TestOneWordTypoPointsAtTheWord(t *testing.T) {
	_, err := ParsePhrase("legal winner thank year wave sausage worth usefull legal winner thank yellow")
	var pe *PhraseError
	if !errors.As(err, &pe) || pe.Word != 8 || pe.Suggest != "useful" {
		t.Fatalf("got %#v", err)
	}
	if !strings.Contains(err.Error(), "word 8") || !strings.Contains(err.Error(), `"useful"`) {
		t.Fatalf("message: %q", err)
	}
	_, err = ParsePhrase("legal winner thank year wave sausage worth uesful legal winner thank yellow")
	if !errors.As(err, &pe) || pe.Word != 8 || pe.Suggest != "useful" {
		t.Fatalf("edit-distance suggestion: %#v", err)
	}
	// Two words swapped: every word is real, the checksum catches it.
	_, err = ParsePhrase("winner legal thank year wave sausage worth useful legal winner thank yellow")
	if !errors.As(err, &pe) || !pe.Checksum {
		t.Fatalf("swap not caught: %v", err)
	}
	_, err = ParsePhrase("legal winner thank")
	if !errors.As(err, &pe) || pe.Count != 3 || !strings.Contains(err.Error(), "3 words") {
		t.Fatalf("count: %v", err)
	}
	_, err = ParsePhrase("  ")
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "no words") {
		t.Fatalf("empty: %v", err)
	}
}

func TestPhraseNeverPrints(t *testing.T) {
	p, err := NewPhrase()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{p.String(), p.GoString()} {
		for _, w := range p.Words() {
			if strings.Contains(s, " "+w+" ") || strings.HasPrefix(s, w+" ") {
				t.Fatalf("%q shows a word", s)
			}
		}
	}
}

func TestGoldenDerivations(t *testing.T) {
	for _, v := range loadVectors(t).Mirrin {
		p, err := ParsePhrase(v.Phrase)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(p.k[:]) != v.Entropy {
			t.Fatalf("entropy %x", p.k)
		}
		if got := hex.EncodeToString(p.derive(infoPQ)); got != v.PQSeed {
			t.Fatalf("pq seed %s", got)
		}
		if got := hex.EncodeToString(p.derive(infoX25519)); got != v.X25519Seed {
			t.Fatalf("x25519 seed %s", got)
		}
		if got := hex.EncodeToString(p.derive(infoRecovery)); got != v.RecoverySeed {
			t.Fatalf("recovery seed %s", got)
		}
		rec := p.Recipient()
		sum := sha256.Sum256([]byte(rec))
		if !strings.HasPrefix(rec, v.RecipientPrefix) || hex.EncodeToString(sum[:]) != v.Recipient {
			t.Fatalf("recipient %s…", rec[:40])
		}
		x, _ := p.X25519Identity()
		if got := x.Recipient().String(); got != v.X25519Recipient {
			t.Fatalf("x25519 recipient %s", got)
		}
		if got := p.RecoveryPub(); got != v.RecoveryPub {
			t.Fatalf("recovery pub %s", got)
		}
		if got := p.Namespace(); got != v.Namespace {
			t.Fatalf("namespace %s", got)
		}
		if ns, err := NamespaceOf(p.RecoveryPub()); err != nil || ns != v.Namespace {
			t.Fatalf("NamespaceOf %s %v", ns, err)
		}
		if !p.Opens(rec) || !p.Opens(v.X25519Recipient) {
			t.Fatal("the words don't open their own recipients")
		}
	}
}

// The strings handed to age must be exactly what age itself writes, or the
// stock tool would reject `mirrin backup key --age`.
func TestDerivedKeysAreCanonicalAge(t *testing.T) {
	p, _ := NewPhrase()
	pq, err := p.AgeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	enc := strings.ToUpper(bech32Encode("AGE-SECRET-KEY-PQ-", p.derive(infoPQ)))
	if pq.String() != enc {
		t.Fatal("hybrid identity encoding differs from age's")
	}
	if _, err := age.ParseHybridRecipient(p.Recipient()); err != nil {
		t.Fatal(err)
	}
	x, err := p.X25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if x.String() != strings.ToUpper(bech32Encode("AGE-SECRET-KEY-", p.derive(infoX25519))) {
		t.Fatal("x25519 identity encoding differs from age's")
	}
	if other, _ := NewPhrase(); other.Opens(p.Recipient()) {
		t.Fatal("other words open these backups")
	}
}

// docs/backup-format.md shows the same golden values the code is held to.
func TestFormatDocShowsTheVectors(t *testing.T) {
	doc, err := os.ReadFile("../../docs/backup-format.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range loadVectors(t).Mirrin {
		p, _ := ParsePhrase(v.Phrase)
		pq, _ := p.AgeIdentity()
		x, _ := p.X25519Identity()
		for _, want := range []string{v.Phrase, v.Entropy, v.PQSeed, v.X25519Seed, v.RecoverySeed, v.RecipientPrefix, v.Recipient,
			v.X25519Recipient, v.RecoveryPub, v.Namespace, KitID(p), pq.String(), x.String()} {
			if !strings.Contains(string(doc), want) {
				t.Errorf("docs/backup-format.md lacks %s", want)
			}
		}
		if got := len(p.Recipient()); !strings.Contains(string(doc), fmt.Sprintf("%d,%03d characters", got/1000, got%1000)) {
			t.Errorf("docs/backup-format.md gives the wrong recipient length (%d)", got)
		}
	}
}
