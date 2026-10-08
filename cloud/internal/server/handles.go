package server

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Handle rules (docs/cloud-api.md §5.2): 3 to 32 of a-z, 0-9 and '-', no
// hyphen at either end and no "--" (which IDNA reserves, so no xn--
// look-alikes). On top of those, a handle may not be a reserved name, look
// like one, contain a brand that phishing would borrow, or look like a
// handle that already exists. Handles are never reassigned.
var (
	errBadHandle      = errors.New("A handle is 3 to 32 of a-z, 0-9 and single hyphens inside.")
	errReservedHandle = errors.New("That handle is reserved, or looks like a name that is.")
)

// checkHandle applies the character rules.
func checkHandle(h string) error {
	if len(h) < 3 || len(h) > 32 || h[0] == '-' || h[len(h)-1] == '-' || strings.Contains(h, "--") ||
		strings.Trim(h, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return errBadHandle
	}
	return nil
}

// reserved are names no handle may take or resemble: infrastructure,
// mail and certificate-validation names, and pages people trust.
var reserved = []string{
	"abuse", "account", "accounts", "acme", "admin", "administrator", "api", "app", "auth",
	"autoconfig", "autodiscover", "billing", "cloud", "dev", "dns", "docs", "ftp", "help",
	"hostmaster", "imap", "info", "isatap", "localhost", "login", "mail", "noc", "ns1", "ns2",
	"pay", "pop", "postmaster", "recovery", "relay", "root", "secure", "security", "signin",
	"smtp", "ssl", "staff", "static", "status", "support", "sysadmin", "test", "tls",
	"update", "verify", "webmaster", "wpad", "www",
}

// brands may not appear anywhere in a handle, however spelled.
var brands = []string{
	"mirrin", "mavrk", "amazon", "apple", "binance", "coinbase", "facebook", "google",
	"icloud", "instagram", "letsencrypt", "metamask", "microsoft", "netflix", "paypal", "whatsapp",
}

// folds turn look-alike sequences and characters into one spelling. The
// sequences go first ("rn" reads as "m").
var (
	seqFolds  = strings.NewReplacer("rn", "m", "vv", "w", "cl", "d")
	charFolds = strings.NewReplacer("0", "o", "1", "l", "i", "l", "3", "e", "4", "a", "5", "s", "7", "t", "8", "b", "9", "g", "-", "")
)

// skeleton is h with look-alikes folded and hyphens dropped. Two handles
// with one skeleton are too alike to coexist.
func skeleton(h string) string {
	return charFolds.Replace(seqFolds.Replace(h))
}

// allowedHandle applies the rules and the reserved, look-alike and brand
// checks. Whether the handle is taken is the store's to say.
func allowedHandle(h string) error {
	if err := checkHandle(h); err != nil {
		return err
	}
	sk := skeleton(h)
	for _, r := range reserved {
		if sk == skeleton(r) {
			return errReservedHandle
		}
	}
	for _, b := range brands {
		if strings.Contains(sk, skeleton(b)) {
			return errReservedHandle
		}
	}
	return nil
}

// Random handles are adjective-noun-NN, from words that pass the rules.
var (
	adjectives = []string{
		"amber", "azure", "brave", "brisk", "calm", "cedar", "coral", "cosy", "crisp", "dusky",
		"ember", "fable", "fern", "fleet", "frost", "gentle", "golden", "hazel", "honey", "ivory",
		"jade", "jolly", "keen", "lucky", "lunar", "maple", "merry", "misty", "noble", "ochre",
		"olive", "pearl", "plucky", "polar", "quick", "quiet", "rapid", "rosy", "rustic", "sable",
		"sage", "sandy", "silver", "snowy", "solar", "sunny", "swift", "tawny", "tidy", "velvet",
		"vivid", "warm", "willow", "witty", "zesty",
	}
	nouns = []string{
		"badger", "beaver", "bison", "crane", "cricket", "dingo", "dolphin", "eagle", "falcon", "ferret",
		"finch", "fox", "gecko", "gibbon", "goose", "hare", "hedgehog", "heron", "ibex", "jackal",
		"koala", "lemur", "lynx", "magpie", "marten", "meerkat", "moose", "moth", "newt", "otter",
		"owl", "panda", "pelican", "penguin", "puffin", "quail", "quokka", "raven", "robin", "salmon",
		"seal", "sparrow", "stoat", "swan", "tapir", "toucan", "trout", "walrus", "wombat", "wren", "yak",
	}
)

// randomHandle picks adjective-noun-NN with crypto/rand.
func randomHandle() string {
	pick := func(n int) int {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
		if err != nil {
			panic(err) // crypto/rand does not fail
		}
		return int(v.Int64())
	}
	return fmt.Sprintf("%s-%s-%02d", adjectives[pick(len(adjectives))], nouns[pick(len(nouns))], pick(100))
}
