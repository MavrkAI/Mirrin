package voice

import "strings"

// spokenMoney says an amount the way people do: "700 dollars", "1 dollar",
// "349 dollars and 30 cents", "99 cents", "5 thousand dollars", "1.2
// million dollars". m is a reMoney match.
func spokenMoney(m string) string {
	g := reMoney.FindStringSubmatch(m)
	whole, dec, scale := g[1], g[2], strings.ToLower(g[3])
	if scale != "" {
		n := whole
		if dec != "" {
			n += "." + dec
		}
		return n + " " + map[string]string{"k": "thousand", "m": "million", "b": "billion"}[scale] + " dollars"
	}
	if dec != "" && len(dec) != 2 { // "$1.5": a decimal, not cents
		return whole + "." + dec + " dollars"
	}
	cents := strings.TrimLeft(dec, "0")
	plain := strings.ReplaceAll(whole, ",", "")
	if plain == "0" {
		if cents == "" {
			return "0 dollars"
		}
		return cents + " " + plural(cents, "cent")
	}
	out := whole + " " + plural(plain, "dollar")
	if cents != "" {
		out += " and " + cents + " " + plural(cents, "cent")
	}
	return out
}

func plural(n, unit string) string {
	if n == "1" {
		return unit
	}
	return unit + "s"
}
