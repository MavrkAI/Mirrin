package browser

import (
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// typeInto puts text in a field in place of what it holds. Clearing by
// keystrokes isn't enough on sites whose fields keep their own copy of the
// value (React and friends): a retry then appended to the old text
// ("AkshayAkshayAkshay"). So the field is emptied through its own value
// setter, typed into with real keystrokes, and checked; if the site still
// holds something else, the value is set directly and the page told.
func typeInto(sel, text string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if err := chromedp.Run(ctx, chromedp.WaitVisible(sel, chromedp.ByQuery), chromedp.Click(sel, chromedp.ByQuery)); err != nil {
			return err
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(setValueJS(sel, ""), nil)); err != nil {
			return err
		}
		if err := chromedp.Run(ctx, chromedp.SendKeys(sel, text, chromedp.ByQuery)); err != nil {
			return err
		}
		var got string
		if err := chromedp.Run(ctx, chromedp.Evaluate(valueJS(sel), &got)); err != nil {
			return err
		}
		if got == text {
			return nil
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(setValueJS(sel, text), nil), chromedp.Evaluate(valueJS(sel), &got)); err != nil {
			return err
		}
		if got != text {
			return fmt.Errorf("the field holds %q instead of %q; check it on the page before going on", got, text)
		}
		return nil
	})
}

// setValueJS sets a field's value the way the page's own code would see it:
// through the element's native setter, then input and change events.
func setValueJS(sel, v string) string {
	return fmt.Sprintf(`(() => { const el = document.querySelector(%q); if (!el) return false;
  const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : el instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
  const d = Object.getOwnPropertyDescriptor(proto, 'value');
  if (el.isContentEditable) { el.textContent = %q; } else if (d && d.set) { d.set.call(el, %q); } else { el.value = %q; }
  el.dispatchEvent(new Event('input', {bubbles: true})); el.dispatchEvent(new Event('change', {bubbles: true})); return true; })()`, sel, v, v, v)
}

func valueJS(sel string) string {
	return fmt.Sprintf(`(() => { const el = document.querySelector(%q); if (!el) return ''; return el.isContentEditable ? el.textContent : String(el.value ?? ''); })()`, sel)
}

// pressKey presses a key or a combination ("Enter", "ArrowDown", "Meta+a",
// "Control+A", "Shift+Tab") as a real key press, never as typed letters.
func pressKey(spec string) (chromedp.Action, error) {
	parts := strings.Split(strings.TrimSpace(spec), "+")
	key := strings.TrimSpace(parts[len(parts)-1])
	var mods input.Modifier
	for _, m := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "meta", "cmd", "command":
			mods |= input.ModifierMeta
		case "control", "ctrl":
			mods |= input.ModifierCtrl
		case "alt", "option":
			mods |= input.ModifierAlt
		case "shift":
			mods |= input.ModifierShift
		default:
			return nil, fmt.Errorf("unknown modifier %q in %q (use Meta, Control, Alt or Shift)", m, spec)
		}
	}
	if k, ok := namedKeys[key]; ok {
		key = k
	} else if k, ok := map[string]string{"Space": " ", "Esc": kb.Escape, "Return": kb.Enter}[key]; ok {
		key = k
	} else if len([]rune(key)) != 1 {
		return nil, fmt.Errorf("unknown key %q (use a single character or a key name like Enter, Tab, Escape, Backspace, ArrowDown)", key)
	}
	if mods != 0 {
		return chromedp.KeyEvent(key, chromedp.KeyModifiers(mods)), nil
	}
	return chromedp.KeyEvent(key), nil
}
