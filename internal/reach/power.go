package reach

import (
	"os"
	"path/filepath"
	"strings"
)

// A desktop without a system power supply entry is mains-powered. A battery
// or mains entry with missing/unreadable state is conservatively not on AC.
// Batteries in a wireless mouse, keyboard or game pad (scope "Device") say
// nothing about the computer's own power, so they are skipped.
func powerSupplyOnAC(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	hasSupply := false
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		if scope, err := os.ReadFile(filepath.Join(dir, "scope")); err == nil && strings.TrimSpace(string(scope)) == "Device" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "type"))
		if err != nil {
			return false
		}
		switch strings.TrimSpace(string(b)) {
		case "Battery":
			hasSupply = true
		case "Mains", "USB", "USB_C", "USB_PD":
			hasSupply = true
			b, err = os.ReadFile(filepath.Join(dir, "online"))
			if err == nil && strings.TrimSpace(string(b)) == "1" {
				return true
			}
		}
	}
	return !hasSupply
}
