package service

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// unitEnv reads the environment an installed launchd plist or systemd unit
// sets. Older versions wrote API keys there; install moves them to the
// secrets file before replacing the unit.
func unitEnv(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if strings.HasSuffix(path, ".plist") {
		return plistEnv(b)
	}
	return systemdEnv(b)
}

// plistEnv returns the EnvironmentVariables dictionary of a launchd plist.
func plistEnv(b []byte) map[string]string {
	dec := xml.NewDecoder(bytes.NewReader(b))
	out := map[string]string{}
	depth, envDepth := 0, -1
	var lastKey, pending string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			text.Reset()
			if t.Name.Local == "dict" && lastKey == "EnvironmentVariables" && envDepth < 0 {
				envDepth = depth
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			switch {
			case t.Name.Local == "key":
				lastKey = text.String()
				if depth == envDepth+1 {
					pending = lastKey
				}
			case t.Name.Local == "string" && envDepth > 0 && depth == envDepth+1 && pending != "":
				out[pending] = text.String()
				pending = ""
			case t.Name.Local == "dict" && depth == envDepth:
				return out
			}
			depth--
		}
	}
}

// systemdEnv returns the Environment= assignments of a systemd unit.
func systemdEnv(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, "Environment=")
		if !ok {
			continue
		}
		if u, err := strconv.Unquote(rest); err == nil {
			rest = u
		}
		if k, v, ok := strings.Cut(rest, "="); ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// rewriteUnitEnv removes the drop variables from a plist's or systemd unit's
// environment and adds the add ones it lacks, keeping everything else as it
// is. The file is replaced atomically with its mode unchanged. It reports
// whether anything changed.
func rewriteUnitEnv(path string, drop []string, add map[string]string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	have := unitEnv(path)
	var out string
	if strings.HasSuffix(path, ".plist") {
		out = rewritePlistEnv(string(b), drop, add, have)
	} else {
		out = rewriteSystemdEnv(string(b), drop, add, have)
	}
	if out == string(b) {
		return false, nil
	}
	return true, writeUnitFile(path, out, st.Mode().Perm())
}

var rePlistEnv = regexp.MustCompile(`<key>EnvironmentVariables</key>\s*<dict>`)

func rewritePlistEnv(s string, drop []string, add, have map[string]string) string {
	for _, k := range drop {
		re := regexp.MustCompile(`\s*<key>` + regexp.QuoteMeta(html.EscapeString(k)) + `</key>\s*<string>[^<]*</string>`)
		s = re.ReplaceAllString(s, "")
	}
	var extra strings.Builder
	for _, k := range sortedKeys(add) {
		if _, ok := have[k]; !ok {
			extra.WriteString("\n\t\t<key>" + html.EscapeString(k) + "</key>\n\t\t<string>" + html.EscapeString(add[k]) + "</string>")
		}
	}
	if extra.Len() == 0 {
		return s
	}
	if loc := rePlistEnv.FindStringIndex(s); loc != nil {
		return s[:loc[1]] + extra.String() + s[loc[1]:]
	}
	if i := strings.LastIndex(s, "</dict>"); i >= 0 { // no environment yet
		return s[:i] + "\t<key>EnvironmentVariables</key>\n\t<dict>" + extra.String() + "\n\t</dict>\n" + s[i:]
	}
	return s
}

func rewriteSystemdEnv(s string, drop []string, add, have map[string]string) string {
	gone := map[string]bool{}
	for _, k := range drop {
		gone[k] = true
	}
	var lines []string
	for _, line := range strings.SplitAfter(s, "\n") {
		if one := systemdEnv([]byte(line)); len(one) == 1 {
			for k := range one {
				if gone[k] {
					line = ""
				}
			}
		}
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if strings.TrimSpace(line) == "[Service]" {
			for _, k := range sortedKeys(add) {
				if _, ok := have[k]; !ok {
					lines = append(lines, "Environment="+k+"="+add[k]+"\n")
				}
			}
		}
	}
	return strings.Join(lines, "")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeUnitFile replaces the service definition at path with s atomically,
// with mode.
func writeUnitFile(path, s string, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(s); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
