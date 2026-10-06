package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Minimal ELF64 headers let every host check both relay architectures.
func relayELF(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], 2)
	binary.LittleEndian.PutUint16(b[18:], machine)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	return b
}

func TestRelayPlatformAndNames(t *testing.T) {
	for _, c := range []struct {
		name    string
		machine uint16
		bad     bool
	}{
		{"mirrin-relay-linux-amd64", 62, false}, {"mirrin-relay-linux-arm64", 183, false},
		{"mirrin-relay-linux-amd64", 183, true}, {"mirrin-relay-linux-arm64", 62, true},
		{"mirrin-relay-darwin-arm64", 183, true}, {"mirrin-relay-linux-arm64.exe", 183, true},
		{"mirrin-relay-linux-universal", 183, true}, {"mirrin-relay-SHA256SUMS", 183, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.name), relayELF(c.machine), 0600); err != nil {
				t.Fatal(err)
			}
			problems, err := checkFixtures(dir, nil)
			if err != nil || (len(problems) > 0) != c.bad {
				t.Fatalf("%v: %v", err, problems)
			}
		})
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mirrin-relay-linux-amd64"), []byte("not ELF"), 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := checkFixtures(dir, nil); err != nil || len(p) == 0 {
		t.Fatalf("accepted invalid relay: %v %v", p, err)
	}
}

func TestVariantsAreRequiredSeparately(t *testing.T) {
	bin := hostBinary(t)
	base := runtime.GOOS + "/" + runtime.GOARCH
	defaultName := assetFor(runtime.GOOS, runtime.GOARCH, "")
	mitName := strings.TrimSuffix(defaultName, ".exe") + "-nowhatsapp"
	if runtime.GOOS == "windows" {
		mitName += ".exe"
	}
	for _, name := range []string{defaultName, mitName} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), bin, 0700); err != nil {
			t.Fatal(err)
		}
		p, err := checkFixtures(dir, []string{base, base + "/nowhatsapp"})
		if err != nil || len(p) != 1 || !strings.Contains(p[0], "missing") {
			t.Fatalf("variant satisfied both requirements: %v %v", p, err)
		}
	}
	dir := t.TempDir()
	for _, name := range []string{defaultName, mitName, "mirrin-relay-linux-amd64", "mirrin-relay-linux-arm64"} {
		data := bin
		if strings.HasPrefix(name, "mirrin-relay-") {
			machine := uint16(62)
			if strings.HasSuffix(name, "arm64") {
				machine = 183
			}
			data = relayELF(machine)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := checkFixtures(dir, []string{base, base + "/nowhatsapp", "relay/linux/amd64", "relay/linux/arm64"}); err != nil || len(p) > 0 {
		t.Fatalf("%v %v", p, err)
	}
	n, err := writeSums(dir)
	if err != nil || n != 4 {
		t.Fatalf("sums: %d %v", n, err)
	}
	sums, err := os.ReadFile(filepath.Join(dir, SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{defaultName, mitName, "mirrin-relay-linux-amd64", "mirrin-relay-linux-arm64"} {
		if !strings.Contains(string(sums), "  "+name+"\n") {
			t.Errorf("missing checksum: %s", name)
		}
	}
	// A full release must contain six of each CLI variant, both relays and
	// the four voice bundles.
	p, err := checkFixtures(t.TempDir(), strings.Split(allPlatforms, ","))
	if err != nil || len(p) != 18 {
		t.Fatalf("full release requirements: %v %v", p, err)
	}
}
