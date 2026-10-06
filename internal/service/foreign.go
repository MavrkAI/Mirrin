package service

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"os"
	"runtime"
	"strings"
)

// ForeignUnit reports a background service installed for another twin: the
// mirrin service definition exists, but for a different MIRRIN_HOME. It gives
// where the definition is and the program it starts ("" when that can't be
// read). That service isn't this home's to remove, and while it is installed
// the program it starts must stay. Windows keeps services in the registry, so
// there it reports none.
func ForeignUnit() (path, program string, ok bool) { return foreignUnit(Name) }

// ForeignLegacyUnit is ForeignUnit for the services from before the rename
// (AntBot's, openHuman's).
func ForeignLegacyUnit() (path, program string, ok bool) {
	for _, name := range legacyNames {
		if path, program, ok = foreignUnit(name); ok {
			return path, program, ok
		}
	}
	return "", "", false
}

func foreignUnit(name string) (string, string, bool) {
	return foreignUnitAt(unitPath(runtime.GOOS, userHome(), name))
}

func foreignUnitAt(path string) (string, string, bool) {
	if path == "" || !fileExists(path) || ownUnit(unitEnv(path)) {
		return "", "", false
	}
	return path, unitProgram(path), true
}

// unitProgram is the program a launchd plist or systemd unit starts, or "".
func unitProgram(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if strings.HasSuffix(path, ".plist") {
		return plistProgram(b)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "ExecStart=")
		if !ok {
			continue
		}
		// kardianos writes the path with its spaces as \x20; a hand-edited
		// unit may quote it instead.
		rest = strings.TrimLeft(rest, "-@:+!")
		if strings.HasPrefix(rest, `"`) {
			if end := strings.Index(rest[1:], `"`); end >= 0 {
				return rest[1 : end+1]
			}
		}
		if f := strings.Fields(rest); len(f) > 0 {
			return strings.ReplaceAll(f[0], `\x20`, " ")
		}
	}
	return ""
}

// plistProgram is the first ProgramArguments entry of a launchd plist (or its
// Program key).
func plistProgram(b []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(b))
	var lastKey string
	var text strings.Builder
	inArgs := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			text.Reset()
			if t.Name.Local == "array" && lastKey == "ProgramArguments" {
				inArgs = true
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			switch t.Name.Local {
			case "key":
				lastKey = strings.TrimSpace(text.String())
			case "string":
				if inArgs || lastKey == "Program" {
					return strings.TrimSpace(text.String())
				}
			case "array":
				inArgs = false
			}
		}
	}
}
