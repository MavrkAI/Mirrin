package phone

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"sort"
)

func sortStrings(s []string) { sort.Strings(s) }
func hmacB64(key, msg string) string {
	m := hmac.New(sha1.New, []byte(key))
	m.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}
