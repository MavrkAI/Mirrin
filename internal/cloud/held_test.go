package cloud

import (
	"path/filepath"
	"sync"
	"testing"
)

// A file replaced while it is being read is read whole, never refused: on
// Windows a refused read of the entitlement once stopped cloud reach as if
// the link had expired.
func TestReadWhileReplaced(t *testing.T) {
	p := filepath.Join(t.TempDir(), entitlementFile)
	if err := writeFileAtomic(p, []byte("v2.public.token\n")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 300 {
			if err := writeFileAtomic(p, []byte("v2.public.token\n")); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 300 {
			b, err := readFileMax(p, 100)
			if err != nil {
				errs <- err
				return
			}
			if string(b) != "v2.public.token\n" {
				t.Errorf("read %q", b)
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
