# Phone app and direct push checks

These features need a secure origin: HTTPS on a phone, or http://127.0.0.1 for a local browser test. Use an isolated `MIRRIN_HOME`, never the owner's real twin. Do not bind port 7742. Automated tests use recorders or a local fake push service; never send a test to a real vendor endpoint.

## Automated checks

Run with `MIRRIN_HOME=$(mktemp -d)` (and a writable temporary `GOCACHE` when necessary):

- `go test ./internal/push ./internal/api ./internal/daemon`
- `go test -race ./internal/push ./internal/api ./internal/daemon ./internal/config`
- `MIRRIN_BROWSER_TESTS=1 go test ./internal/api -run TestPWABrowserLifecycle -count=1`

The browser test uses a temporary profile and `--use-mock-keychain`. It checks service worker readiness, installability errors, the twin's manifest name, a worker version change and cache retirement, offline navigation, and cache contents. It requires local sockets and Chrome. `TestPWAWorkerWithoutSockets` runs the real worker in Node with fake browser interfaces: integrity verification, offline fallback, version replacement, private-route exclusions, quiet resolved replacements, badge updates and safe tap URLs.

`TestPWAClientWithoutSockets` checks standalone-only ticket redemption, keeping an existing cookie, and one-time 428 retries with the original request body.

`TestRFC8291` compares the published section 5 ciphertext and Appendix A intermediate values. `TestSendRecorder` and `TestSendHTTP` independently decrypt with the receiver's key and inspect request headers. No vendor is contacted. `TestPushApprovalDecisionAndBadge` exercises the daemon's direct approval hook and a loopback decision.

## Manual matrix (required; not yet run)

| Target | Install and launch | Offline reload | Push within 5 seconds | Tap and resolved badge | Status |
|---|---|---|---|---|---|
| iOS 26, Safari/Home Screen | Required | Required | Required | Required | Unverified |
| iOS 27, Safari/Home Screen | Required | Required | Required | Required | Unverified |
| Android, Chrome | Required | Required | Required | Required | Unverified |
| Desktop, Chrome | Required | Required | Required | Required | Unverified |
| Desktop, Edge | Required | Required | Required | Required | Unverified |
| macOS, Safari | Required | Required | Required | Required | Unverified |

Record exact OS/browser versions, twin build, HTTPS route, elapsed delivery time and screenshots with each result. Run against the same origin throughout.

1. Set a distinctive twin name. Pair a phone using `mirrin pair --screen`. On the screen, choose **Install**. On iOS, open the **…** menu, then **Add to Home Screen**. Check the app name and icon.
2. On Android and desktop, check that the manifest start URL stays `/ui`. On iOS Safari, its ticket start URL is `/start#t=it_…`. Fetching `/start` or the manifest must not redeem it. Launch from the Home Screen within 30 minutes; it should obtain its own cookie once. An ordinary Safari tab must never redeem it. Reopening the installed app should work without another redemption. Used/expired tickets should give a next step.
3. Choose **Enable notifications** in the Home Screen app, then allow the OS prompt. Verify that denying permission gives a useful next step. Test subscription deletion and device revocation.
4. While connected, load the screen. Turn off the network and reload. Expect “Can't reach <Name> — last seen …”, a retry link and no enabled approval controls or message outbox. Cache Storage may contain only the public shell assets: never events, screen responses, messages, approvals, cookies, tickets or push subscriptions.
5. Reconnect, change a shell asset/build and update the service worker. A new `mirrin-shell-…` cache must activate only after its SHA-256 hashes pass, and older shell caches must disappear. A corrupt or interrupted install must leave the old worker usable.
6. Raise an approval. Check the lock screen within five seconds. The notification must have no decision buttons. Tap it: the target is `/approve/<id>`. Until WP-06 mounts its focused approval page, this route redirects to the existing authenticated screen at `/ui#approval=<id>` and focuses the card. The focused page and passkey flow remain planned in WP-06.
7. Decide on loopback or a second device. Other devices must receive a resolved push with the original tag and the current pending count; the deciding device must not receive an old queued approval. Test two pending approvals so the badge falls from two to one.
8. Try full, brief and private previews. Private approval, question and security notifications say only “<Name> needs you”. Resolved notifications say “<Name>: handled” and “This request no longer needs you.”; test notifications say “Notifications are ready.” Quiet hours suppress ordinary questions/tasks, but approvals, resolutions, security alarms and test pushes still arrive. Repeat with a revoked device and confirm it receives nothing new.
9. Check update/restart behavior: the VAPID public key and subscriptions persist, while pending retries do not survive restart. Existing pending approvals remain available on the screen.

## Integration contracts and remaining checks

- `window.mirrinPWA` exposes `standalone()`, `install()`, and `notifications()`. WP-06 sets `mirrinPWA.stepUp = async response => boolean`; a successful step-up retries a cloned request once, while cancellation preserves the 428. The shim never makes a decision itself.
- `Daemon.PushSecurity(id, summary)` is the direct alarm entry point for reach/certificate producers. Those producers are outside this work package and absent from this worktree. Task questions use the task notification callback, approvals use `agent.OnApproval`, and neither uses the lossy event bus.
- `push` config supports `enabled` (defaults true), `subject` (defaults the project HTTPS URL), `preview` (defaults brief), `quiet_hours` (`22:00-07:00`, daemon local time), and `kinds` (empty means all). Notification subscriptions live in `data/push.json`, VAPID in `data/vapid.pem`, both private files. Security alarms and resolved messages bypass kind filters; approvals bypass quiet hours.
- The offline shell reports this browser's last successful screen fetch. Relay status URLs are currently empty: reach/status integration is planned in its owning package.
- The two-minute, eight-character “Finish pairing” fallback code remains planned with the pairing/devices UI; expired install tickets currently instruct the owner to pair again.
- Real iOS declarative delivery, five-second vendor delivery and all OS/browser matrix entries require manual confirmation. A socket-restricted sandbox cannot establish these results.

Protocol references: [RFC 8291 example](https://www.rfc-editor.org/rfc/rfc8291#section-5), [WebKit declarative push format](https://webkit.org/blog/16535/meet-declarative-web-push/).

On Android and desktop Chrome, repeat approval/resolve cycles and check that resolved pushes quietly replace the same tag, with no generic “updated in the background” notification. Check that resolved taps open the screen and private test pushes say “Notifications are ready.”
