package browser

import (
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Teach mode watches the owner fill in real forms, and some of what they type
// must never be written down: passwords, card numbers, security codes,
// one-time codes and identity numbers. Those steps are recorded as
// [secret: the user types this]; the replay hands the window back to the
// owner for them.

// secretValue stands in for a value that was not recorded.
const secretValue = "[secret: the user types this]"

// field describes the input a value was typed into, as the recorder sees it.
type field struct {
	Type         string `json:"type"`
	Autocomplete string `json:"ac"`
	Name         string `json:"name"`
	ID           string `json:"id"`
	InputMode    string `json:"mode"`
	Label        string `json:"label"`
	Placeholder  string `json:"ph"`
	Aria         string `json:"aria"`
}

// secretWords are words in a field's name, id or label that mark it secret.
var secretWords = map[string]bool{
	"password": true, "passwd": true, "pwd": true, "passcode": true, "passphrase": true, "pin": true,
	"otp": true, "totp": true, "hotp": true, "2fa": true, "mfa": true,
	"cvv": true, "cvv2": true, "cvc": true, "cvc2": true, "csc": true, "cvn": true, "ccv": true,
	"ccnum": true, "ccnumber": true, "cardnumber": true, "cardnum": true, "cardno": true, "creditcard": true, "debitcard": true,
	"iban": true, "ssn": true, "sin": true, "nino": true, "tfn": true, "aadhaar": true, "aadhar": true, "pan": true,
	"passport": true, "secret": true, "securitycode": true, "onetimecode": true, "cardholder": true,
	"expiry": true, "expiration": true, "sortcode": true, "token": true, "texted": true, "authenticator": true,
	"smscode": true, "authcode": true, "otpcode": true, "mfacode": true, "verifycode": true, "verificationcode": true,
	"logincode": true, "signincode": true, "accesscode": true, "confirmationcode": true,
}

// secretPhrases are runs of words that mark a field secret.
var secretPhrases = []string{
	"card number", "card no", "card num", "credit card", "debit card", "name on card", "card holder",
	"cc number", "cc num", "cc exp", "cc csc", "cc cvv", "cc cvc", "cc name",
	"security code", "verification code", "one time", "auth code", "authentication code", "sms code", "access code",
	"expiry date", "expiration date", "exp date", "valid thru", "expiry month", "expiry year", "card expiry",
	"account number", "account no", "acct number", "acct no", "routing number", "sort code", "bank account",
	"social security", "national id", "national insurance", "tax id", "tax number", "tax file", "id number",
	"driver license", "drivers license", "driver licence", "drivers licence", "driving licence", "driving license",
	"licence number", "license number", "security answer", "secret answer", "memorable word",
	// A code sent to the owner's phone or app, however the field is named.
	"we sent", "sent to your", "two factor", "2 step", "two step", "login code", "log in code", "sign in code",
	"confirmation code",
	// Card expiry split into month and year, and the CVV by another name.
	"exp month", "exp year", "card exp", "expiration month", "expiration year", "security number",
}

// codeWords mark a field that may take a one-time code: "Enter the 6-digit
// code", "Code". A short run of digits typed into one is masked. (A PIN or
// token field is masked whatever is typed.)
var codeWords = map[string]bool{"code": true}

// notSecretCode are words that make a "code" field an everyday one: a promo
// code, a postcode, a dialling code.
var notSecretCode = map[string]bool{
	"promo": true, "promotion": true, "promotional": true, "coupon": true, "discount": true, "voucher": true, "gift": true,
	"referral": true, "invite": true, "invitation": true, "postal": true, "post": true, "zip": true, "area": true,
	"dial": true, "dialing": true, "dialling": true, "country": true, "tracking": true, "currency": true,
	"language": true, "lang": true, "locale": true, "region": true, "product": true, "item": true, "sku": true,
	"error": true, "status": true, "reason": true, "campaign": true, "airport": true, "station": true,
}

// secretField reports whether a value typed into f must not be recorded.
func secretField(f field, value string) bool {
	if strings.EqualFold(f.Type, "password") {
		return true
	}
	for _, tok := range strings.Fields(strings.ToLower(f.Autocomplete)) {
		if strings.HasPrefix(tok, "cc-") || tok == "one-time-code" || strings.HasSuffix(tok, "-password") {
			return true
		}
	}
	code, everyday := false, false
	for _, s := range []string{f.Name, f.ID, f.Label, f.Placeholder, f.Aria} {
		ws := words(s)
		for _, w := range ws {
			if secretWords[w] {
				return true
			}
			code = code || codeWords[w]
			everyday = everyday || notSecretCode[w]
		}
		joined := " " + strings.Join(ws, " ") + " "
		for _, p := range secretPhrases {
			if strings.Contains(joined, " "+p+" ") {
				return true
			}
		}
	}
	if code && !everyday && oneTimeCode(value) {
		return true
	}
	return secretText(value) || numericCode(f, value)
}

// oneTimeCode is 4 to 8 digits, as a code sent by text or an authenticator
// app is ("123 456" and "123-456" too).
func oneTimeCode(v string) bool {
	compact := strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(v))
	return len(compact) >= 4 && len(compact) <= 8 && allDigits(compact)
}

// numericCode is a long run of digits typed into a field made for numbers:
// a card, an account or an ID number. (Phone fields are left alone.)
func numericCode(f field, value string) bool {
	if !strings.EqualFold(f.InputMode, "numeric") && !strings.EqualFold(f.Type, "number") {
		return false
	}
	compact := strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(value))
	return len(compact) >= 12 && allDigits(compact)
}

// secretText reports a value that is a secret whatever the field: a card
// number (13 to 19 digits passing the Luhn check), a US social security
// number or an IBAN.
func secretText(v string) bool {
	v = strings.TrimSpace(v)
	if reSSN.MatchString(v) {
		return true
	}
	if isIBAN(v) {
		return true
	}
	compact := strings.NewReplacer(" ", "", "-", "").Replace(v)
	return len(compact) >= 13 && len(compact) <= 19 && allDigits(compact) && luhn(compact)
}

var reSSN = regexp.MustCompile(`^\d{3}-\d{2}-\d{4}$`)

// reCardRun finds digit runs (with spaces or dashes) long enough to be a card.
var reCardRun = regexp.MustCompile(`\d(?:[ -]?\d){12,18}`)

// scrubCards replaces card numbers anywhere in recorded text (a label, a
// selector, an address).
func scrubCards(s string) string {
	return reCardRun.ReplaceAllStringFunc(s, func(m string) string {
		if luhn(digitsOnly(m)) {
			return "[card number]"
		}
		return m
	})
}

// luhn is the card-number checksum.
func luhn(d string) bool {
	if len(d) < 2 {
		return false
	}
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if n < 0 || n > 9 {
			return false
		}
		if double {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

var reIBAN = regexp.MustCompile(`^[A-Z]{2}\d{2}[A-Z0-9]{11,30}$`)

// isIBAN checks an international bank account number (ISO 13616, mod 97).
func isIBAN(v string) bool {
	v = strings.ToUpper(strings.ReplaceAll(v, " ", ""))
	if !reIBAN.MatchString(v) {
		return false
	}
	var b strings.Builder
	for _, r := range v[4:] + v[:4] {
		if r >= 'A' && r <= 'Z' {
			b.WriteString(strconv.Itoa(int(r-'A') + 10))
		} else {
			b.WriteRune(r)
		}
	}
	n, ok := new(big.Int).SetString(b.String(), 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func allDigits(s string) bool { return s != "" && digitsOnly(s) == s }

// words splits an identifier or label into lower-case words: "cardNumber",
// "card_number" and "Card number" all give "card", "number".
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			// A lower-to-upper change starts a word: cardNumber.
			if i > 0 && unicode.IsUpper(r) && unicode.IsLower(rs[i-1]) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// secretParams are URL parameters that carry a sign-in, a one-time code or a
// session: their values are not recorded.
var secretParams = map[string]bool{
	"code": true, "token": true, "auth": true, "authorization": true, "key": true, "api_key": true, "apikey": true,
	"otp": true, "password": true, "pass": true, "pwd": true, "secret": true, "sid": true, "sig": true,
	"ticket": true, "nonce": true, "state": true, "samlresponse": true, "samlrequest": true, "jwt": true,
}

// secretParamParts are parts of a parameter name that mark it secret:
// oauth_token, login_token, access-token, oauth_verifier, X-Amz-Signature,
// PHPSESSID.
var secretParamParts = []string{"token", "secret", "password", "passwd", "signature", "verifier", "credential", "session", "sessid"}

// plainParams name what kind of secret is next to them, not a secret.
var plainParams = map[string]bool{"token_type": true, "token_type_hint": true}

// secretParam reports whether a URL parameter's value must not be recorded.
func secretParam(name string) bool {
	n := strings.ToLower(name)
	if secretParams[n] {
		return true
	}
	if plainParams[n] {
		return false
	}
	for _, p := range secretParamParts {
		if strings.Contains(n, p) {
			return true
		}
	}
	// auth_code, otpCode, verificationcode; not promo_code or zipcode.
	ws := words(name)
	for _, w := range ws {
		if notSecretCode[w] || notSecretCode[strings.TrimSuffix(w, "code")] {
			return false
		}
	}
	return len(ws) > 0 && strings.HasSuffix(ws[len(ws)-1], "code")
}

// scrubURL makes an address safe to keep: the user name and password in it,
// the values of sign-in and session parameters (query and #fragment), a
// sign-in token in the path (/reset-password/<token>) and any card number are
// hidden.
func scrubURL(raw string) string {
	base, frag, hasFrag := strings.Cut(raw, "#")
	out, query, hasQuery := strings.Cut(base, "?")
	out = scrubPath(dropUserinfo(out))
	if hasQuery {
		out += "?" + scrubParams(query)
	}
	if hasFrag {
		out += "#" + scrubParams(frag)
	}
	return scrubCards(out)
}

// dropUserinfo removes "user:password@" from "https://user:password@host/p".
func dropUserinfo(s string) string {
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return s
	}
	host, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		host = host[i+1:]
	}
	return scheme + "://" + host + path
}

// signInWords in a path mark a long random segment in it as a sign-in link's
// token rather than a document's ID (which a routine needs to find the
// document again).
var signInWords = map[string]bool{
	"reset": true, "password": true, "passwd": true, "verify": true, "verification": true, "confirm": true,
	"confirmation": true, "activate": true, "activation": true, "magic": true, "login": true, "signin": true,
	"sign": true, "auth": true, "oauth": true, "token": true, "invite": true, "invitation": true, "recover": true,
	"recovery": true, "unlock": true, "otp": true, "session": true, "callback": true, "sso": true, "saml": true,
}

// scrubPath replaces sign-in tokens in the path of "scheme://host/path" with
// [secret]: a JSON web token anywhere, and on a sign-in page (a path that
// says reset, verify, magic, login…) any segment of 20 or more random
// letters and digits.
func scrubPath(s string) string {
	start := 0
	if i := strings.Index(s, "://"); i >= 0 {
		start = i + 3
		if j := strings.IndexByte(s[start:], '/'); j >= 0 {
			start += j
		} else {
			return s
		}
	}
	segs := strings.Split(s[start:], "/")
	signIn := false
	for _, seg := range segs {
		if tokenLike(seg) {
			continue // a random string can spell anything
		}
		for _, w := range words(seg) {
			signIn = signIn || signInWords[w]
		}
	}
	for i, seg := range segs {
		if strings.HasPrefix(seg, "eyJ") && len(seg) >= 20 || signIn && tokenLike(seg) {
			segs[i] = "[secret]"
		}
	}
	return s[:start] + strings.Join(segs, "/")
}

// tokenLike is a path segment that looks random: 20 or more base64url or hex
// characters with a long stretch mixing letters and digits (not a slug like
// "iphone-15-pro-max-256gb").
func tokenLike(seg string) bool {
	if len(seg) < 20 {
		return false
	}
	for _, r := range seg {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '=') {
			return false
		}
	}
	for _, piece := range strings.FieldsFunc(seg, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == '=' }) {
		if len(piece) >= 12 && strings.ContainsAny(piece, "0123456789") && strings.IndexFunc(piece, unicode.IsLetter) >= 0 {
			return true
		}
	}
	return false
}

// scrubParams hides secret values in "a=1&b=2".
func scrubParams(q string) string {
	parts := strings.Split(q, "&")
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		name, err := url.QueryUnescape(k)
		if err != nil {
			name = k
		}
		if ok && secretParam(name) {
			parts[i] = k + "=[secret]"
		}
	}
	return strings.Join(parts, "&")
}
