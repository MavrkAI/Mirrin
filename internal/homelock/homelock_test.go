package homelock

import (
	"errors"
	"os"
	"testing"
)

func TestInUseSeesTheClaim(t *testing.T) {
	dir := t.TempDir()
	if InUse(dir) {
		t.Fatal("a home never claimed is in use")
	}
	if _, err := os.Stat(Path(dir)); err == nil {
		t.Fatal("looking created the claim's file")
	}
	f, err := Lock(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !InUse(dir) {
		t.Fatal("a held claim isn't in use")
	}
	if _, err := Lock(Path(dir)); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second lock: %v", err)
	}
	f.Close()
	if InUse(dir) {
		t.Fatal("still in use after the claim was given back")
	}
}
