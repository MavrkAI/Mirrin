package server

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func tun(gen int64, iat time.Time, hosts ...string) *Tunnel {
	t := newTunnel()
	t.Gen, t.Iat, t.Hostnames = gen, iat, hosts
	return t
}

// Per hostname the higher (gen, iat) wins: a lower gen is superseded, the
// same gen with an older iat is told to refresh and retry, and a tie goes
// to the newcomer.
func TestRegistryOrdering(t *testing.T) {
	t0 := time.Unix(1_790_000_000, 0)
	r := NewRegistry()
	a := tun(3, t0, "h.test")
	if d, err := r.Attach(a); err != nil || d != nil {
		t.Fatalf("first attach: %v %v", d, err)
	}
	if _, err := r.Attach(tun(2, t0.Add(time.Hour), "h.test")); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("lower gen: %v", err)
	}
	if _, err := r.Attach(tun(3, t0.Add(-time.Second), "h.test")); !errors.Is(err, ErrSupersededRetry) {
		t.Fatalf("same gen, older iat: %v", err)
	}
	if got, _ := r.Lookup("h.test"); got != a {
		t.Fatal("a refused tunnel took the name")
	}
	tie := tun(3, t0, "h.test")
	if d, err := r.Attach(tie); err != nil || !slices.Equal(d, []*Tunnel{a}) {
		t.Fatalf("tie: %v %v", d, err)
	}
	newer := tun(3, t0.Add(time.Minute), "h.test")
	if d, err := r.Attach(newer); err != nil || !slices.Equal(d, []*Tunnel{tie}) {
		t.Fatalf("same gen, newer iat: %v %v", d, err)
	}
	higher := tun(4, t0.Add(-time.Hour), "h.test")
	if d, err := r.Attach(higher); err != nil || !slices.Equal(d, []*Tunnel{newer}) {
		t.Fatalf("higher gen: %v %v", d, err)
	}
	// A displaced tunnel detaching later must not take the name with it.
	r.Detach(newer)
	if got, ok := r.Lookup("h.test"); !ok || got != higher {
		t.Fatal("a stale detach removed the holder")
	}
	r.Detach(higher)
	if _, ok := r.Lookup("h.test"); ok {
		t.Fatal("detached name still routes")
	}
}

// A tunnel carries a set of names and is attached all or nothing. One that
// outranks two holders displaces both, and they lose every name they held.
func TestRegistrySets(t *testing.T) {
	t0 := time.Unix(1_790_000_000, 0)
	r := NewRegistry()
	a := tun(1, t0, "a.test", "x.test")
	b := tun(1, t0, "b.test")
	r.Attach(a)
	r.Attach(b)
	if _, err := r.Attach(tun(1, t0.Add(-time.Second), "b.test", "c.test")); !errors.Is(err, ErrSupersededRetry) {
		t.Fatalf("partial loser: %v", err)
	}
	if _, ok := r.Lookup("c.test"); ok {
		t.Fatal("a refused tunnel was partly attached")
	}
	// A higher gen anywhere wins over a retry elsewhere.
	r.Attach(tun(5, t0, "d.test"))
	if _, err := r.Attach(tun(1, t0.Add(time.Hour), "a.test", "d.test")); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("mixed: %v", err)
	}
	c := tun(2, t0, "a.test", "b.test")
	d, err := r.Attach(c)
	if err != nil || len(d) != 2 {
		t.Fatalf("displaced %v, %v", d, err)
	}
	if _, ok := r.Lookup("x.test"); ok {
		t.Fatal("a displaced tunnel kept a name")
	}
	for _, h := range []string{"a.test", "b.test"} {
		if got, _ := r.Lookup(h); got != c {
			t.Fatalf("%s not on the new tunnel", h)
		}
	}
}
