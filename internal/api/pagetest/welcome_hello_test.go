package pagetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The first hello says what the twin is looking at until its words come,
// then shows them with the weather's small print, and the link on keeps
// talking on the screen. Before voice is set up the Mac's standard voice
// says it, and the checkbox says so; once it is, the twin says it itself.
func TestWelcomeHelloInItsOwnWords(t *testing.T) {
	const (
		line = "Good evening, Akshay. Fourteen degrees and clear in Melbourne. Say my name or type, whenever you like."
		note = "Weather comes from Open-Meteo for your time zone's city, never your exact location."
	)
	for _, voiceReady := range []bool{false, true} {
		t.Run(fmt.Sprint("voice ready ", voiceReady), func(t *testing.T) {
			ctx, d, _ := welcomeStep2(t, voiceReady)
			label := "Say it out loud (your Mac's standard voice until voice is set up)"
			if voiceReady {
				label = "Say it out loud"
			}
			waitFor(t, ctx, `document.querySelector('#speaklabel').textContent === `+jsString(label), "the checkbox's label")
			release := make(chan struct{})
			var (
				mu     sync.Mutex
				posted map[string]any
			)
			d.handle("POST /welcome/hello", func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				_ = json.Unmarshal(raw, &posted)
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(event string, v any) {
					b, _ := json.Marshal(v)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
					w.(http.Flusher).Flush()
				}
				send("status", "Looking at your evening…")
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				send("delta", line)
				send("note", note)
				send("done", line)
				if voiceReady {
					send("spoke", true)
				}
			})
			run(t, ctx, chromedp.Evaluate(`window.__spoken = []; speechSynthesis.speak = u => __spoken.push(u.text); true`, nil))
			run(t, ctx, chromedp.SendKeys("#you", "Akshay", chromedp.ByQuery), chromedp.Click(`#nameform > button`, chromedp.ByQuery))
			waitFor(t, ctx, `document.querySelector('#reply').textContent === 'Looking at your evening…'`, "what it's looking at")
			close(release)
			waitFor(t, ctx, `document.querySelector('#reply').textContent === `+jsString(line), "the hello")
			waitFor(t, ctx, visible("#fine")+` && document.querySelector('#fine').textContent === `+jsString(note), "the small print")
			if voiceReady {
				time.Sleep(300 * time.Millisecond) // the stream has ended: nothing more to say
				if n := eval[int](t, ctx, `__spoken.length`); n != 0 {
					t.Fatalf("the Mac's voice spoke over the twin's %d times", n)
				}
			} else {
				waitFor(t, ctx, `__spoken.length === 1 && __spoken[0] === `+jsString(line), "the Mac's voice saying it")
			}
			mu.Lock()
			speak := posted["Speak"]
			mu.Unlock()
			if speak != true {
				t.Fatalf("hello posted %v", posted)
			}
			if href := eval[string](t, ctx, `document.querySelector('#hello a').getAttribute('href')`); href != "/ui?hello=1" {
				t.Fatalf("Keep talking goes to %q", href)
			}
		})
	}
}

// From the welcome's Keep talking, the screen waves once and the address
// drops ?hello=1.
func TestScreenHelloFromTheWelcome(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, desktop(), chromedp.Navigate(d.url("/ui?hello=1")))
	waitFor(t, ctx, `document.querySelector('#talkOrb').classList.contains('hi')`, "the wave")
	if search := eval[string](t, ctx, `location.search`); search != "" {
		t.Fatalf("the address still says %q", search)
	}
}
