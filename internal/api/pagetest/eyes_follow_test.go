package pagetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// The penguin turns to look at the pointer, mostly left and right (its
// head turning further than its body), and goes back to peeking about when
// it stops.
func TestThePenguinTurnsToThePointer(t *testing.T) {
	d := newDaemon(t)
	d.handle("GET /screen", jsonH(screen(nil)))
	ctx := tab(t)
	run(t, ctx, chromedp.EmulateViewport(1440, 900), chromedp.Navigate(d.url("/ui")))
	waitFor(t, ctx, loaded, "loaded")
	var box []float64
	run(t, ctx, chromedp.Evaluate(`(() => { const r = document.querySelector('#talkOrb .eyes').getBoundingClientRect(); return [r.left + r.width/2, r.top + r.height/2]; })()`, &box))
	move := func(x, y float64) {
		run(t, ctx, chromedp.ActionFunc(func(ctx context.Context) error { return input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx) }), chromedp.Sleep(150*time.Millisecond))
	}
	turn := `document.querySelector('#talkOrb').style.getPropertyValue('--lx')`
	move(box[0]-400, box[1]) // far to its left
	left := eval[string](t, ctx, turn)
	move(box[0]+500, box[1]+40) // far to its right
	right := eval[string](t, ctx, turn)
	if !strings.HasPrefix(left, "-1") || !strings.HasPrefix(right, "1") {
		t.Fatalf("turned %q looking left, %q looking right", left, right)
	}
	if !eval[bool](t, ctx, `document.querySelector('#talkOrb').classList.contains('follow') && !!document.querySelector('#talkOrb .head') && !!document.querySelector('#talkOrb .turn')`) {
		t.Fatal("not turning")
	}
}
