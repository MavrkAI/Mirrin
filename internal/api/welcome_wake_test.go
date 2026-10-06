package api

import (
	"encoding/json"
	"testing"
)

// wakeCheckFake is a welcome backend that knows which wake models are here.
type wakeCheckFake struct {
	welcomeFake
	here map[string]bool
}

func (f *wakeCheckFake) HasWakeModel(model string) bool { return f.here[model] }

// A build without the "Hey Mirrin" model still promised that Mirrin hears
// his name first. The page now asks the twin which models are here.
func TestWelcomePersonasAskWhetherTheWakeModelIsHere(t *testing.T) {
	for _, here := range []bool{false, true} {
		e := newEnv(t)
		e.s.WithWelcome(&wakeCheckFake{here: map[string]bool{"hey_mirrin.onnx": here}})
		w := e.do(onLoopback, req{path: "/welcome/personas", header: bearer(master)})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		var got []WelcomePersona
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Fatal("no personas")
		}
		for _, p := range got {
			if want := here && p.ID == "mirrin"; p.InstantWake != want {
				t.Errorf("model here %v: %s instant %v", here, p.ID, p.InstantWake)
			}
		}
	}
}
