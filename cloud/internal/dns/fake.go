package dns

import (
	"context"
	"net/netip"
	"slices"
	"sync"
)

// Records are one handle's record sets.
type Records struct {
	A, AAAA []netip.Addr
	CAA     []CAA
}

// Fake keeps records in memory, for --dev mode and tests.
type Fake struct {
	mu      sync.Mutex
	records map[string]Records
	// Fail, when set, makes every call fail with it, as an outage would.
	Fail error
}

// NewFake returns an empty fake zone.
func NewFake() *Fake { return &Fake{records: map[string]Records{}} }

// UpsertHandle implements Provider.
func (f *Fake) UpsertHandle(_ context.Context, handle string, v4, v6 []netip.Addr, caa []CAA) error {
	if err := Label(handle); err != nil {
		return err
	}
	if err := checkAddrs(v4, v6); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail != nil {
		return f.Fail
	}
	f.records[handle] = Records{A: slices.Clone(v4), AAAA: slices.Clone(v6), CAA: slices.Clone(caa)}
	return nil
}

// DeleteHandle implements Provider.
func (f *Fake) DeleteHandle(_ context.Context, handle string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Fail != nil {
		return f.Fail
	}
	delete(f.records, handle)
	return nil
}

// SetFail makes calls fail with err from now on, or succeed again if nil.
func (f *Fake) SetFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Fail = err
}

// Records returns a handle's records, and whether it has any.
func (f *Fake) Records(handle string) (Records, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[handle]
	return r, ok
}
