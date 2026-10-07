# Pictures for the README

| File | What it is | How it's made |
| --- | --- | --- |
| `banner.png` | The banner at the top: the name, the line and Mirrin. | `site/assets/og-card.html` (the site's social card), drawn at 2x. |
| `presence-dark.png`, `presence-light.png` | The real presence screen on a busy afternoon, with sample data. The README shows the one that matches the reader's theme. | `MIRRIN_UI_SHOTS=<dir> go test ./internal/api/pagetest -run TestPresenceScreenShots` writes `desktop-dark.png` and `desktop-light.png`; copy them here. |
| `cast.png` | The three personas a fresh install comes with. | `cast-card.html`, drawn at 2x. Its portraits are `site/assets/characters/`, which `draw.js` makes from the screen's own code. |

To draw a card at 2x, use headless Chrome with a throwaway profile and the mock keychain (never your own profile):

```sh
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new \
  --use-mock-keychain --user-data-dir="$(mktemp -d)" --hide-scrollbars \
  --force-prefers-reduced-motion --force-device-scale-factor=2 \
  --window-size=1200,440 --screenshot="$PWD/docs/media/cast.png" \
  "file://$PWD/docs/media/cast-card.html"
```

The banner is the same with `--window-size=1200,630` and `site/assets/og-card.html`. Draw them again after a rename, a new persona or a change to a drawing.
