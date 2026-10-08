# Changelog

All notable changes are listed here. The format follows Keep a Changelog; versions follow SemVer.

## Unreleased

### The cast is Mirrin, Nyra and Pickoo
- The bundled persona `plain`, drawn as the coral rover, has retired before release. Nothing to do: a config.yaml that chose it (`persona: plain`) means Mirrin now, by his name, answering to "Hey Mirrin" in his own voice, and the next save writes `mirrin`. A name you gave your twin yourself stays.

### MAVRK is now Mirrin
- The default persona, the gentleman who's two steps ahead, takes the product's name: he is Mirrin, and answers to "Hey Mirrin" (and to the ways transcription tends to spell it: Mirin, Mirren, Miran, Mirron, Mirrin's). Same character, drawing and voice; he still calls you sir.
- Nothing to do: a config.yaml that says `persona: mavrk` or `name: MAVRK` still means him, by his new name, and the next save writes `mirrin`. The wake word "maverick" that earlier releases wrote there gives way to "mirrin".
- He uses a `hey_mirrin.onnx` wake-word model when one is in the voice folder; until then he hears his name in what is transcribed. An old `hey_maverick.onnx` stays in the folder and in backups, but it listens for the old name, so he doesn't use it.

### Governance
- MavrkAI plans an optional paid availability service. Nothing free today will move behind it; the twin never pitches it. See [the accepted design](docs/cloud-design.md).
- The source stays MIT. Downloads with WhatsApp link GPL-3.0 code, so they are GPL-3.0 as a whole; builds made with `-tags nowhatsapp` are MIT. Both contain yamux (MPL-2.0), which carries the relay tunnel, and the downloads with WhatsApp also contain whatsmeow and go.mau.fi/util (MPL-2.0): those modules' own files keep their licence, and their source is in every release. THIRD_PARTY_NOTICES lists every module in the programs, `mirrin-relay` too, and the BIP-39 wordlist the 12 recovery words come from (MIT), and opens by saying what each build is; `mirrin licenses` says which one you have.
- The code will be published as a new repository, github.com/MavrkAI/Mirrin, from a checked copy: no trained wake-word model and no internal planning or launch drafts, and nothing goes up until the copy has no private details, no secret beyond known test vectors, and builds. How it's done is in [the release guide](docs/maintainers-release.md). The Cloud design keeps its architecture; its business planning is kept with the maintainers, and the site's answer to what Cloud costs to run now says what would run, without estimates.

### Mirrin Cloud — built, planned, not for sale
- The optional paid service is in the code (the relay, signed account permissions, and account, billing and DNS services) but is not running and not for sale: this version trusts no Cloud signing key, so `mirrin cloud link` stops before any checkout. [docs/cloud.md](docs/cloud.md) says what it would do, and [docs/cloud-trust.md](docs/cloud-trust.md) what it could and couldn't see, each with a command to check.
- The cloud client stays idle until you explicitly run `mirrin cloud link`. Its request ledger and tests guard the promise that an unlinked twin makes no cloud or relay requests, and core features do not depend on payment.
- Everything the service would sell has a free route in this release: HTTPS reach over Tailscale, your own certificate or your own relay, and backups to a folder, iCloud Drive or any S3 bucket. The phone app, lock-screen approvals and Face ID are free on every route.
- Backups kept with the paid service are built, and coming, like reach: `mirrin backup target cloud` or `mirrin backup init --cloud`, listed after the free places, on a linked machine. Only ciphertext is sent, straight to storage at short-lived signed addresses; the service counts sizes and names, keeps 7 daily, 4 weekly and 6 monthly snapshots, and while payment has lapsed takes nothing new but lets you fetch what is there for 90 days (the backup then names the free places instead). An upload counts against your space from the moment it is allowed, so ones that never finish can't pile up; the service removes them itself, and a deleted backup is gone from storage before the service answers. `mirrin restore --from cloud` on a new machine needs only your 12 words: the account moves there, and the old machine stands by. `mirrin backup migrate --from <place> --to <place>` copies every backup between any two places (folders, iCloud Drive, S3 buckets, the service), byte for byte, and checks each copy; it says which older ones the service doesn't keep, and running it again copies nothing twice.
- The daemon's side of reach through the paid service is built, and coming: this version trusts no signing keys yet, so the new Reach page (Reach from anywhere… in the menu) lists it last, as coming, after Tailscale and your own relay. Once linked (`mirrin reach use cloud`), the twin keeps a tunnel to both relays at once, gets its own certificate for its address, tells the service which certificate account the address's CAA record must name, and stands by if another machine takes the address over, but only once the service confirms it (asking again until it answers). A copy standing by after a backup handover won't start a checkout. Phone calls use that address when `phone.public_url` is empty.

### Security
- Every device gets its own key. `mirrin pair` makes a single-use code (with `--screen`, a link and QR code for a phone or tablet; with `--kiosk`, a view-only link for a wall screen) that works once, for 10 minutes, and never holds this computer's master key; `mirrin connect` takes it. Screens, phones and terminals paired the old way keep working and move onto keys of their own; revoking one of them also changes the old shared key.
- You're told whenever a device is paired, with a one-line way to cut it off if it wasn't you: in your own chat, or on the presence screen and as a desktop notification.
- A paired device does only what it was paired for. It talks only in its own front end's chat, never in your WhatsApp or other apps' chats. One paired without approve can't answer a request by typing "yes", confirm a "yes, always", or decide one by saying "stop" (the request stays waiting for you); one paired to talk can't clear the conversation every screen shares, reload protocols or retry a task, and one without view can't read the activity log or spending. A wall screen paired to look shows no text box and no Approve/Deny.
- The pages the menu opens no longer keep the master key in a browser cookie. Other websites, including ones on other localhost ports, can't post to the twin through your browser, and proxied requests to the local address are refused. Settings pages (Channels, Accounts, Memory, Protocols) open only on the computer the twin runs on. If another program holds 127.0.0.1 on the API's port, the twin says so instead of sending it the master key.
- Approvals remember how risky they were and who said yes (a named phone, the screen, a chat, in `yes N` or your own words), and a request whose details changed after you were asked is never carried out as if you'd seen it. Rows written without that check (by an older version, or brought in by an import) get it at the next start.
- "Yes, always" is taken only from your own chat app or `mirrin chat`: never out loud (anyone in the room could say it), never by email, IRC or in a group chat, never for browser clicks or private memories, and never for anything that can't easily be undone. A watcher's run, which acts on what others wrote in an email or invite, still asks before anything that changes something, whatever `always_allow` says.
- A transcribed voice note is never taken as approval, however it's worded ("yes 1", "yep, send it"); a spoken no still counts. Out loud, the twin no longer asks you to say "yes N" for a dangerous action: it says where to approve it instead.
- The browser keeps to the public internet like `fetch_url`: pages, redirects, images and WebSockets on this computer, the home network or a cloud metadata service are refused unless listed in `skills.web.allow_hosts` (one list for both, applied as soon as you change it), and file:// and other non-web addresses are refused.
- An approved click or form submission goes ahead only if the page and the exact element you approved are unchanged (also when you approved it in your own words), and a yes more than a day later is refused.
- Teach mode never writes down passwords, card numbers, security codes, one-time codes, ID numbers or sign-in tokens in addresses; the replay hands the window back to you for them. A web page can't write steps of its own into a recording.
- The presence screen serves only the screenshot a waiting approval shows, not every screenshot of a signed-in page kept for the model.
- What email senders and calendar invites write reaches the model as quoted information, never as instructions. A subject or address can no longer carry an extra header such as a Bcc.
- Dangerous actions (running commands, paying) are never approved by a voice in the room: the request goes to your phone and the presence screen.
- The saved ElevenLabs key is no longer put into the environment, so programs the twin starts can't see it.
- The problem report (`mirrin report`) hides the backup's keys, and a mistyped Recovery Kit word never reaches the log or the report. Logs hide age keys by their shape.
- Google Connect is refused, in words, from a page that isn't on this computer, where the sign-in could never finish.
- Email: mail counts as yours only when your provider's own check (SPF, DKIM or DMARC) passes for you, so a forged From address is ignored. For a provider Mirrin doesn't recognise, or a local bridge such as Proton's, set `skills.email.auth_servers`. The Channels page says when mail in your name was ignored, and why.
- IRC: you're recognised by your NickServ account or a full `nick!user@host` mask, never by a nick anyone can borrow. Old prefix masks still work, and the Channels page warns when a mask would accept anyone.
- WhatsApp: only your "message yourself" chat counts as you talking to the twin. What you send other people is never taken as an instruction.
- People other than you (`reply_to_others`) never see your memory, and everything the twin would do for them waits for your `yes N`, asked in your own chat.
- Reading SSH keys, cloud credentials, keychains, browser profiles, `.env` files or Mirrin's own files counts as dangerous, so it asks first even if the tool is in `always_allow`, and says why.
- Telegram's bot token no longer appears in logs or on the Channels page.
- Approving by voice never reads out another chat's reply, and approving from the screen no longer also texts WhatsApp.
- Approvals show everything they're for: the whole command, script, message or file, and a protocol's schedule and instructions. By voice, the twin points you to the screen instead of reading a script aloud.
- Creating or running a protocol asks first (add `run_protocol` to `autonomy.always_allow` to skip that). Tools the twin writes for itself count as at least writes, so they ask before every run whatever risk they claim.
- Custom tools and MCP servers no longer see your API keys and tokens. Name what a tool needs under `env:` in its `tool.yaml`; in MCP config, `$NAME` passes a variable through.
- `fetch_url` stays on the public internet, through a proxy too. Add a home server to `skills.web.allow_hosts`.
- API keys are no longer written into the launchd or systemd file. They live in `~/.mirrin/secrets.env`, which only you can read; keys an older version put in the service file move there on the next start.
- `mirrin identity export` leaves out every secret, including passwords inside URLs and tokens inside MCP commands. Archives are readable only by you.
- Packs install only from https, ssh or a folder on your machine. Symbolic links inside a pack are never followed, and removing a pack can only delete a pack.
- A web page or email can no longer make the twin read a local file by naming it as an image. Screenshots only go to your own chat.
- Google sign-in: each Connect is its own single-use, expiring flow with PKCE.

### Fixed
- Typing on a page handed to you on the presence screen arrives in order, so a fast-typed email or code no longer comes out jumbled, and pasting (a password from your password manager) works there.
- Google's sign-in, and Gmail or another Google page that needs you to sign in, opens in an ordinary Chrome window on your computer, which Google accepts, when the twin hands it to you from voice or the terminal. Your login is kept for the twin. If the screen isn't working for you, say so and the twin opens a window instead.
- Voice: the other spellings a persona answers to wake it after "Hey," too, as transcription often writes it: "Hey, Picku" wakes Pickoo, as "Hey, Pickoo" already did.
- Partial configuration files no longer enable WhatsApp implicitly or require an owner number. WhatsApp stays on when explicitly enabled in your settings or setup.
- After `mirrin backup resume` and the restart it asks for, the twin is no longer still paused. A pause you set yourself before the standby is kept.
- `mirrin uninstall` no longer makes a `~/.mirrin` folder (and then refuses to delete it) on a machine with no twin, and on Windows it can delete the twin instead of stopping part-way on its own open log files. `mirrin restore` holds nothing open in the home it moves aside either.
- Connect Google works when Mirrin listens on a Tailscale address, and the sign-out message links the Accounts page where it opens (this computer).
- Model use is counted under the day where you are now, so today's spend and the budget notices are right after a flight.
- A restore no longer tells you every device must pair again: each keeps its own key, and the device review says so. A restored twin keeps this machine's pause and doesn't redo routines the old machine had already run. A restored config from before settings were layered keeps following the system's time zone on a new machine.
- Saying "stop" while the twin answers a voice note names what it stopped, instead of `“”`. A voice note that says "stop" stops the turn it would have waited behind, and "Hey Maverick, stop" (or the menu bar's new Stop what it's doing) stops the twin at once instead of queuing behind what it's doing.
- A photo that answers a background task started from another chat is named to the task as one it can't see, instead of being shown to the chat model and lost.
- The presence screen says why the calendar is empty when Google signed the twin out, instead of "Nothing on the board.", and a screen on another device shows what the twin and your other screens said.
- The Channels page shows a channel that's reconnecting as reconnecting, since when, instead of as a failure.
- Approving one step of a background task no longer ends the task early: it carries on from its board.
- Voice: a quick "yes", "no", "sure" or "stop" right after the twin asks something is heard, also in a `mirrin voice` session joined to the menu bar twin. The twin only speaks once it has confirmed you said its name, acknowledges you the moment you finish, and no longer drops your request when the full transcript spells its name differently. A name only wakes the twin as a whole word ("Pickooing?" no longer wakes Pickoo). "Stop" over the twin stops it talking; its own "Hold on" heard back from the speaker doesn't.
- Voice: if ElevenLabs or Kokoro fails, a backup voice speaks and says so once. If the microphone or speaker stops working you get one plain notification instead of silence. On Windows the wake-word helper no longer crashes waiting for a follow-up.
- Voice: with the "Hey Maverick" model missing from the voice folder, the twin no longer wakes to "Hey Jarvis", openWakeWord's stock model. It listens for its name in what it transcribes, a beat slower, until `mirrin voice setup` puts the model back.
- WhatsApp media no longer arrives as `[image] caption`, and uncaptioned photos and voice notes are no longer met with silence.
- Google no longer goes quiet after a week: when Google ends the sign-in, the twin tells you once, with the exact fix, and the self-check and Accounts page say the same. The general self-check notice no longer repeats it.
- One email the mail server won't hand over no longer stalls the mail channel. Replies thread properly, and non-ASCII subjects and names are encoded.
- The Protocols page's Run now button works, and says whether the protocol actually started. A device that isn't paired gets a clear message saying how to pair it; a settings page names the menu item that opens it.
- Approving from the screen shows the twin's reply once, not twice. Long errors wrap on a phone instead of pushing the page sideways.
- Channels heal themselves: one that drops reconnects on its own. A rejected token or a missing permission stops it and says what to fix, and you're told on another channel. The Channels page and `mirrin doctor` say truthfully whether each one is connected, reconnecting or failed.
- Messages you send while the twin is busy are answered in order, never dropped, and other chats don't wait.
- Saying yes: "Yes.", "ok, go ahead", "no thanks" and 👍 all count, but a bare yes only answers the one thing the twin just asked you. Anything less certain gets "just to be sure", with the request shown as stored.
- An approval is carried out once, however many times it's answered (a double click, the orb and the screen together).
- `yes N` works for approvals from protocols, the watcher and background tasks, on the presence screen, and on your phone when a task you started in the terminal asks there.
- Reminders, approval requests and task updates reach you on your next connected channel, then as a desktop notification, when the usual one can't deliver. You can answer an approval where it arrived.
- Memory: every turn sees your newest facts and the ones related to what you said, not just the oldest 150. `recall` puts the best matches first and says when nothing matches.
- `forget` means forget: the fact is also cleared from the call that stored it, its approval, the activity log, recall results, the twin's word-for-word repeats of it, deleted space on disk and the daily memory copies. Your own messages are left alone.
- Background tasks: cancelling stops the work in progress, a restart no longer marks running tasks as failed, and a task that runs out of steps pauses and says what's left. No more false "Done."
- Reminders set during a routine, a background task or a call now arrive, and retry with backoff when a channel is down.
- One broken persona, protocol or schedule no longer switches the twin back to MAVRK or stops the other routines. `mirrin protocols check` and `mirrin persona list` say which file and why.
- Switching persona brings its wake word, detector and voice with it, and they stay after a restart. Ones you set yourself stay put.
- `mirrin identity import` no longer leaves a config.yaml that won't load. Importing again repairs one.
- `mirrin service install` starts Mirrin straight away, waits until it answers, and shows the last error in plain words if it doesn't. Running it again replaces the old install cleanly.
- Only one Mirrin runs per home. A second copy says so and quits; the background service waits its turn.
- `mirrin doctor` really checks the model, says whether Mirrin is running, and exits 1 when something is wrong.
- A key added with `mirrin init`, or at the prompt `mirrin chat` now shows, is used at once with no restart. The menu bar no longer saves a provider with no key, or undoes your own edits to config.yaml.
- A busy API port no longer stops Mirrin; it tries again every 30 seconds and the health page says why.
- install.sh picks the right build on every Mac and Linux machine (Apple silicon under Rosetta too), explains what went wrong in plain words, and keeps your working copy if the new one won't start. The Intel Mac download is really a Mac program now.
- The email channel leaves other mail unread; only mail it answers is marked read.
- Protocols that need email work with Gmail as well as IMAP. `mirrin protocols check` no longer reports vars you set in vars.yaml as missing, and `mirrin protocols update` exits with an error when a pack couldn't be checked or updated.
- Approved actions run to the end even if the page that approved them closes. An approved browser action runs only if the browser is still on the page it was asked on, and shows as not done if it isn't.
- `mirrin voice` without a running Mirrin starts listening, and says what's missing when it can't. The menu bar's Talk and Chat work when `mirrin` isn't on your PATH.
- OpenAI project keys that can't list models are accepted when they can chat.
- Signal linked to your own account works (talk to the twin in Note to Self). iMessage ignores tapbacks, reactions and group chats, and a link you send is no longer mistaken for a file. Telegram and IRC keep non-Latin text whole in long replies, and a long IRC answer no longer drops the connection. WhatsApp no longer leaks a database handle while offline.
- `mirrin persona new` and `mirrin protocols new` never overwrite a file.
- Anthropic: large `max_tokens` values and MCP tools with `oneOf`/`anyOf` schemas work. OpenAI-compatible providers accept turns with a screenshot and several tool calls.
- Settings changed from the menu bar apply from the next turn. Turning autonomy down takes effect at the very next tool call, even mid-task.
- The Docker image turns healthy, and a name or bio with a colon or `#` no longer breaks first boot.
- A data folder with `#`, `?` or `%` in its name opens the right memory database.

### Changed
- Mirrin no longer ships the "Hey Maverick" wake-word model. It was trained on openWakeWord's ACAV100M features, which are CC BY-NC-SA 4.0, and macOS voices, so it isn't cleared for commercial use; a licence-clean one is planned (WP-24). Until then MAVRK hears his name the way the other personas do, in what is transcribed on your computer, a beat slower, and voice setup and the Health page say so in a line. A model already in `~/.mirrin/tts` keeps working, and encrypted backups now keep `hey_maverick.onnx` like any other wake-word model; [docs/wake-word.md](docs/wake-word.md) shows how to train your own. Choosing the on-device detector for a name with no model no longer listens for "Hey Jarvis" in its place, and voice setup no longer downloads openWakeWord's example models (CC BY-NC-SA 4.0). While no model is there for your twin's name, nothing uses openWakeWord, so if it doesn't install, setup says so in a line and still finishes.
- Voice notes on WhatsApp, Telegram and Discord are transcribed on your computer with whisper and answered as text marked "(voice note)"; photos go to the model as images, resized and kept only in your data folder for 30 days (and removed with a conversation you `/forget`). Anything else, or on other channels, still gets a quick reply saying it can't be opened there; a caption still comes through. If speech-to-text isn't installed, the twin says what to install. The microphone and voice notes read `channels.voice.language` the same way (unset means English), and a burst of voice notes is transcribed one at a time.
- WhatsApp shows its real connection state on the Channels page and in the health check. When the phone unlinks this computer, it stops, says how to pair again, and the twin tells you on your other channels.
- On messaging apps, a request that takes more than about 8 seconds gets one "On it, this one needs a minute." in the persona's voice.
- Routines keep time like a butler: after the laptop sleeps, each missed routine runs once with a note saying it's late and why; long-missed ones are listed rather than run, and reminders due during sleep arrive together with their times. Morning and evening nudges slept through by more than three hours wait for the next day.
- The twin follows your time zone when you travel and tells you once when it changes; reminders, calendar and email times follow, and a zone you pinned is kept. Pause survives restarts, and resuming sends one summary of what came due.
- config.yaml now holds only what you changed, so updates bring improved defaults. Settings that decide what the twin may do, spend, reach or keep (autonomy, spending limits, shell access, `allow_hosts`, the API address, retention, whether backups carry the WhatsApp session) are always written down, so an update can never loosen them. Two programs saving it at once (the menu and `mirrin backup init`) no longer lose each other's change.
- If your model provider retires your model, the twin keeps working on the provider's current default, tells you once, and switches for good only when you say yes.
- Hearing keeps whisper's model loaded (whisper-server, on a private address; `MIRRIN_WHISPER_SERVER=0` in `secrets.env` turns it off), and whisper-cli takes over within seconds if it stops answering. Voice setup shows progress, resumes an interrupted download, checks every file, and ends with an honest summary; a first setup on a computer that isn't in English gets the multilingual model. `mirrin doctor` checks hearing, the microphone and speaking for push-to-talk users too.
- The browser follows this computer's proxy settings, and says on the Health page when it can't (a PAC script, proxy discovery). Its screenshots are kept 14 days and 200 MB at most. The headless browser presents Chrome's real version. A page that won't load says why in plain words.
- The presence screen on a phone keeps the text box at the bottom without zooming the page, shows replies in full with Copy, and names the day for events in the next few days. Every settings page shows loading, empty and error states with Try again. Keyboard use and colour contrast meet WCAG AA, and reduced motion stops all animation.
- The activity log is kept 90 days, and after 30 days its quotes and long tool results in chats are cut short (`retention:` in config.yaml).
- memory.db records its schema version and upgrades itself step by step; it is checked at start, and a damaged one stops the twin before anything changes, with `mirrin memory restore` to go back to the newest good copy. The problem report calls those daily copies "Memory copies" so they aren't mistaken for the encrypted backups.
- Connecting Google starts watching your calendar and Gmail inbox right away. The watcher speaks up only about new mail, retries a change it couldn't evaluate, and a source that was down for a day starts fresh. Email reads like email: decoded, with quoted history and signatures set aside and attachments listed. Google setup on the Accounts page puts publishing the app before Connect.
- Mac releases are signed and notarized once the Apple credentials are set, install.sh keeps macOS's quarantine flag on a notarized app, and every GitHub Action is pinned to a commit.
- Model errors are explained in plain words with the next step (bad key, out of credit, rate limit, model not pulled, Ollama not running) instead of raw JSON.
- `mirrin init` can be run again to change or repair your setup.
- Moving a twin keeps the new machine's folders, voice models and API address. `mirrin identity import` checks the whole archive first, saves what it replaces in `~/.mirrin/backups/<time>`, lists any keys still to set, and refuses while the twin is running.
- Pack personas are named `<pack>/<id>` (for example `starter/butler`), so a pack can never replace yours or a bundled one. Older configs still find them.
- With replies to others on, at most three people are answered at a time on each channel, and your own messages never wait behind them.
- Protocol lint looks for real credentials instead of the words "password" and "secret", and checks a pack's personas.
- Spending currency and the phone voice follow your country.
- Conversation logs go to `~/.mirrin/logs` instead of the terminal.
- Presence screen, redesigned: type scales with the display so it reads from across the room; a priority line in the header (what needs you, what is running, what is next); approvals and task questions together under "Needs you", the first one large, new ones highlighted, screenshots enlargeable; Today as a timeline with a "now" marker and "in 25 min" for the next item; tasks show progress, the current step and errors, and expand to goal, steps and notes; the last few exchanges sit under the orb with a box to type to the twin; the header shows "Reconnecting" when the daemon is unreachable; footer links to Health, Memory, Channels, Accounts and Protocols; stacked layout below 900px; refreshes never flicker; ambient mode also shows weather and background tasks.
- Voice: sentences no longer run into each other (a breath after each, longer after a full stop), the accent follows the Kokoro voice, voices can be blended (`bf_emma+af_heart`), and markdown, links, list markers, currency, times and symbols become words before synthesis. Acknowledgements are a touch slower. The installed helper script is refreshed from the binary on start.
- `/message/stream` accepts the page cookie as well as the bearer token, so the presence screen can talk to the twin.

### Added
- Nyra joins MAVRK and Pickoo: warm and composed, she calls you by name and is drawn on the screen as a woman in a navy blazer. Choose her in the Welcome window, from the Persona menu or with `mirrin persona use nyra`, and say "Hey Nyra".
- Reach your twin from anywhere over HTTPS, free: `mirrin reach use tailscale` (a `ts.net` certificate; Tailscale's HTTPS Certificates must be on), `mirrin reach use files <cert> <key>`, or `mirrin reach use relay <wss-url> --hostname <name>` through your own `mirrin-relay`. TLS ends on your machine with a key only it holds, and only device keys work from afar. `mirrin reach status`, `verify` (from outside; exits non-zero on a mismatch) and `fingerprint` check it; `mirrin reach stay-awake on` keeps the computer awake on power. [docs/reach.md](docs/reach.md) has the lot.
- With your own relay, your computer gets and renews its own certificate through the tunnel, pinned by a CAA record to its own account. It watches the public certificate logs and CAA every 6 hours; a certificate it didn't ask for pauses approvals from other devices, signs out devices that used the name since then, drops passkeys set up since then and tells you everywhere. `mirrin reach alarm clear`, or `/alarm clear` in your own chat, ends it.
- `mirrin-relay`, the relay, for your own server: one binary or container, SNI passthrough, an allow list you write, no MavrkAI host involved ([docs/relay-selfhost.md](docs/relay-selfhost.md)). Releases include its builds.
- Add your phone (menu bar): one QR code for the best route, with each step lit up as it happens: paired, installed as an app, notifications, Face ID, a test notification. `mirrin devices add` opens it; `mirrin pair --screen` still prints a link.
- Lock-screen approvals: your machine encrypts each notification and sends it straight to Apple's, Google's, Mozilla's or Microsoft's push service, with no server of ours in between. Tapping opens a focused page with the picture and big Approve and Deny buttons; a request decided anywhere clears everywhere. `push.preview`, `push.quiet_hours` and `push.kinds` in config.yaml.
- Face ID for the risky ones: approving a payment, a shell command or a call from a phone needs its passkey, made for that one request. This computer and `yes 12` in your own chat work as before. `reach.step_up: write` or `all` asks for more.
- Devices, Backup, Reach and Trust pages in the menu bar. Trust lists every kind of connection the twin can make, marked on or off, with the certificate other devices see and how to check it yourself.
- Back up to any S3-compatible bucket (AWS S3, Cloudflare R2, Backblaze B2, MinIO, Wasabi): `mirrin backup init --s3 s3://bucket/folder` or `mirrin backup target s3 …`, and `mirrin restore --from s3://…`. The bucket's key is read from variables you name, never written to config.yaml. [docs/backup.md](docs/backup.md) is the guide.
- A Welcome window takes a new install to a first reply: it finds a model key or Ollama you already have, or checks one you paste, names the twin, and can restore an existing twin from a backup instead.
- Photos and voice notes on Slack, Matrix, Mattermost, Signal and iMessage.
- The menu bar opens logs, makes a problem report, shows this month's spend and opens Devices and Backup. Over the monthly model budget, background work pauses until the budget allows; you can still talk to the twin.
- Every release has two builds per platform: the default with WhatsApp (GPL-3.0 as a whole) and an MIT build without it.
- Encrypted backups: `mirrin backup init` makes 12 words and a printable Recovery Kit; backups go to iCloud Drive or any folder, every night at 03:30 and soon after a device is paired or revoked or a fact is forgotten, end-to-end encrypted with age (ML-KEM-768 + X25519). `mirrin restore` brings your twin back here or on a new machine, checks every file first and keeps the old home aside; the machine it moved from stands by until `mirrin backup resume`. `mirrin backup now | status | list | verify | key --age | target | prune`. Snapshots already made keep a forgotten fact until they age out.
- `mirrin devices` lists, renames and revokes paired devices (also with the twin stopped, which asks for a backup at the next start); `/revoke` in your own chat does the same. A public `/healthz` answers health checks.
- Requests you don't answer lapse after 3 days; a late "yes 12" says so, and a background task waiting on one is set aside with a note on how to pick it up again.
- Answer in your own words: after "Which one?", "the landlord one" works, and so does "yep, send it". Payments and commands still need "yes N".
- "Yes, always" stops the twin asking about that kind of action, after a one-line check; "ask me first about send email" undoes it.
- Say "stop" on any channel and the twin stops at once, even an approved action it is carrying out, and says what it stopped; "cancel that" and "never mind" stop what's running in that chat. The menu bar has Stop what it's doing.
- Answer a background task's question from any of your chats, even hours later.
- `mirrin update` installs the latest release when you ask (checked against the release's signed checksums, tried before it's swapped in); `--check` only looks, and `--version` installs a particular one. `mirrin uninstall` removes only what is Mirrin's, keeps your twin unless you type "delete", offers to save a copy, and says your encrypted backups stay. `mirrin licenses` prints the licences; releases include THIRD_PARTY_NOTICES.txt, the full source and an SBOM, and `make build TAGS=nowhatsapp` builds without WhatsApp and the GPL code it needs.
- Every command keeps a log at `~/.mirrin/logs/mirrin.log` (rolled at 5 MB, keys taken out), and crashes go to `logs/crash.log`. If the menu bar app can't start, a desktop notice says why and what fixes it.
- `mirrin report` writes a problem report to read and attach to a bug report, with keys, names and messages left out. `mirrin usage` shows what the model has cost today and this month, as estimates; `usage.monthly_budget` (default US$25) warns at 80% and when it's passed.
- `mirrin calendar login` uses PKCE and a random state.
- One-line install on Windows: `irm https://raw.githubusercontent.com/MavrkAI/Mirrin/main/install.ps1 | iex`.
- Releases now come with a `SHA256SUMS` file signed with Sigstore. The installers check each download against it before installing anything, and install.sh also checks the signature when cosign is installed.
- Native builds for Intel Macs; Mirrin.app is universal.
- Running install.sh again upgrades mirrin where it already is and, when Mirrin runs as a service, tells you to run `mirrin service restart`.
- Packs are pinned to the commit they were installed at. `mirrin protocols update` shows which files changed and asks before applying (`--yes` skips the question), and `mirrin protocols add <url>#<tag|commit>` pins a version.
- Pack search works offline, from a copy of the index built into Mirrin.
- Daily memory copies in `data/backups` (not encrypted, on the same disk); the last seven are kept.
- "typing…" on Telegram, Discord, Signal, Matrix, Mattermost and Zulip while the twin works on a reply.
- After a two-way phone call you're told how it went, including the last thing said.
- A Code of Conduct and a release runbook for maintainers.

## 0.2.0 — 2026-09-23

### Added
- Browser as hands: a persistent signed-in Chrome, screenshots the model sees, `browser_signin` handoff, teach mode that records a task once.
- Accounts page: Google in one click (Calendar, Gmail, Drive).
- Background tasks with a board, pauses for the user, and restart recovery.
- Spending limits and ledger; payment clicks always ask.
- Custom tools the twin writes for itself with approval.
- Phone: SMS and two-way calls through Twilio.
- Channels page with click-and-go setup; WhatsApp pairs in-page by QR or phone code.
- Eight new channels: Discord, Slack, Signal, Matrix, Mattermost, Zulip, IRC, email.
- Zero-config first run.
- Demo video with audio; SECURITY.md, threat model, issue forms, Dependabot; Homebrew formula.

### Changed
- Voice: continuous audio player, brisker acknowledgements, Haiku-safe provider options.

## 0.1.0 — first public cut
- Go daemon with menu bar, wake word, local voice, WhatsApp/Telegram/iMessage, protocols, personas, identity export, presence screen.
