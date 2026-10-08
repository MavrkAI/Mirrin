# Skill setup

## WhatsApp

Mirrin joins your own WhatsApp as a linked device, the same mechanism as WhatsApp Web. You talk to it in the chat with yourself, and its own messages land there too.

Menu bar → **Channels…** → WhatsApp → enter your number → **Pair by scanning a QR code** (or **Get a code to type instead**, which gives an eight-character code for Linked devices → Link with phone number). The daemon does the pairing itself, waits for WhatsApp to finish registering the device, starts the channel and says hello in your chat. `mirrin whatsapp login` does the same from a terminal and hands off to the running daemon so the two never fight over the session.

Only your chat with yourself counts as you talking to the twin: a linked device also sees what you send other people, and none of that is ever taken as an instruction.

Notes: this uses the unofficial multi-device protocol via whatsmeow. It is stable and widely used, but WhatsApp can change it. Keep `reply_to_others: false` unless you want Mirrin answering your contacts. If the phone ever logs the device out, the card shows it and Pair again.

## Voice

Ears: [whisper.cpp](https://github.com/ggerganov/whisper.cpp) with a ggml model, kept loaded in a `whisper-server` on a private address when one is installed next to `whisper-cli` (set `MIRRIN_WHISPER_SERVER=0` in `secrets.env` to always use `whisper-cli`); recording via `sox` (`rec`). Voice notes from chat apps go through the same whisper (`internal/transcribe`), one at a time; `MIRRIN_WHISPER_URL` in `secrets.env` points them at another whisper server, or `off`. `channels.voice.language` applies to both: unset means English. Mouth, in order of preference (`engine: auto`): your own `tts_command`, ElevenLabs if `ELEVENLABS_API_KEY` is set, [Kokoro](https://huggingface.co/hexgrad/Kokoro-82M) if installed, else the OS voice (`say`, `spd-say`/`espeak`, SAPI). If the chosen voice fails mid-reply (ElevenLabs out of quota, the network down, Kokoro's helper crashing), the sentence isn't skipped: Kokoro stands in for ElevenLabs when it's installed, else the system voice, which says it's the backup before its first sentence. You're told once, and the failed voice is left alone for five minutes before it's tried again.

`mirrin voice setup` downloads `ggml-small.en.bin` (small is markedly more accurate than base for names and numbers, still sub-second on Apple Silicon); the first setup on a computer set to a language other than English gets the multilingual `ggml-small.bin` with `channels.voice.language: auto` instead, and says how to pin one language for better accuracy, and installs Kokoro into `~/.mirrin/tts`: a Python 3.12 venv (via `uv` if present, else `python3 -m venv`) with `kokoro-onnx`, plus the 325 MB model and 28 MB voice pack. Kokoro runs as a helper process that keeps the model loaded, so a sentence takes roughly half a second. Voices: `bf_emma` is the most natural British voice, `af_heart` the most natural overall (American); `bm_george`, `bm_fable` (British male), `bf_isabella`, `am_michael`, `af_bella` are also fine. Set `channels.voice.voice`, or blend two: `bf_emma+af_heart`, weighted `bf_emma*0.7+af_heart*0.3`. The accent follows the voice. Each sentence is synthesised on its own with a breath-sized pause after it (longer after a full stop than a comma), and markdown, links, list markers, currency and times are turned into words before they reach the voice.

macOS: `brew install whisper-cpp sox`. Linux: build whisper.cpp and `apt install sox libsox-fmt-all speech-dispatcher`. Windows: install whisper.cpp and sox, or set `record_command` / `tts_command` to your own tools (the recorder must write 16 kHz mono WAV to `$OUT`; the speaker gets text on stdin and in `$TEXT`).

```yaml
channels:
  voice:
    enabled: false            # true to run alongside WhatsApp under the service
    mode: push                # or wake
    wake_word: mirrin
    whisper_model: ~/.mirrin/models/ggml-base.en.bin
    engine: auto              # auto | kokoro | elevenlabs | system | command
    voice: bm_george          # Kokoro voice, or a macOS / ElevenLabs voice name
    speed: 1.0
    followup_seconds: 8
    vocabulary: [Priya, Nguyen, RMIT]   # names whisper should expect
```

**Wake word.** Mirrin ships no wake-word model (`docs/wake-word.md` says why), so out of the box every name, "Hey Mirrin" included, is heard by transcribe-and-match: whisper transcribes each speech segment on your computer and the name is matched in the text, a beat slower than a detector. With a model for the name, openWakeWord (installed by `mirrin voice setup`, in the same Python environment as Kokoro) runs on-device instead: it scores every 80 ms of audio against the model (for Mirrin, `~/.mirrin/tts/hey_maverick.onnx` when one is there) and only then captures the utterance for whisper. Whisper confirms the wake phrase from its own clip before the twin says anything: the chime means "heard something", and the spoken acknowledgement waits for the name, so a false wake from the television gets no "Hello." to the room. `wake_engine: transcribe` uses transcribe-and-match even when a model is there. `wake_threshold` (default 0.5) trades false accepts for misses. To use another phrase, train a model per `docs/wake-word.md` and set `wake_model` to the .onnx path.

**Microphone permission on macOS.** The first time Mirrin opens the mic, macOS asks. If it was already running when you clicked Allow, it keeps hearing silence until restarted: choose Restart from the menu bar (Mirrin notifies you when this happens). Rebuilding an unsigned binary changes its code hash, so macOS would ask again after every `make install`. Run `make sign-identity` once to create a local signing certificate; `make install` then signs the binary and the permission persists across rebuilds. The first signed build asks one more time.

Models: `ggml-base.en.bin` is fast and fine for commands; `ggml-small.en.bin` is more accurate. Grant microphone access to your terminal the first time. For a better built-in macOS voice, download "Daniel (Enhanced)" under System Settings → Accessibility → Spoken Content → System Voice; Mirrin picks it up automatically.

## Telegram

1. Message @BotFather, `/newbot`, copy the token. Export it as `TELEGRAM_BOT_TOKEN` (or put it in `channels.telegram.token`).
2. Find your numeric user id (message @userinfobot) and set `channels.telegram.owner` to it, or use your `@username`.
3. `channels.telegram.enabled: true`, restart, and message your bot. Only the owner is answered unless `reply_to_others` is on.

A bot that already sends its messages to a webhook can't be read; the card says so, and removing the webhook (Telegram's `deleteWebhook`) fixes it.

## iMessage (macOS)

Mirrin reads `~/Library/Messages/chat.db` and replies through the Messages app. Recommended: sign the Mac's Messages into a dedicated Apple ID for Mirrin, then set `channels.imessage.owner` to your own phone number or email and text it from your phone. Grant Full Disk Access to `mirrin` (System Settings → Privacy & Security → Full Disk Access), and Automation access to control Messages when prompted.

## Connecting channels (the easy way)

Menu bar → **Channels…** opens a page with a card per platform. Each card has the setup steps with links (Slack's link creates the app pre-configured from a manifest; Discord shows the invite link once the bot is connected; WhatsApp pairs right on the page with a QR or a typed code), the two or three fields it needs, and a **Connect** button that saves the config, tests the connection and starts the channel live, no restart. A bad token comes back as an error on the card and the channel stays off. Connected cards get a **Send me a test message** button and a link that opens the right chat. The sections below are the same information for people who prefer `config.yaml`.

What every channel does once it's on:

- **Heals itself.** A channel that can't connect, or drops later, reconnects on its own with backoff. A rejected token, a wrong password or a missing permission stops it instead; the card says exactly what to fix, and if the channel had been working you're told on another one. The card and `mirrin doctor` show connected, reconnecting (with the reason) or failed.
- **Drops nothing.** Messages you send while the twin is busy wait their turn and are answered in order; other chats don't wait. "typing…" shows on WhatsApp, Telegram, Discord, Signal, Matrix, Mattermost and Zulip while it works on a reply.
- **Opens what it can.** On WhatsApp, Telegram, Discord, Slack, Matrix, Mattermost, Signal and iMessage, voice notes are transcribed on this computer and answered as "(voice note)" (a transcript is never taken as a yes), and photos go to the model, kept 30 days in the data folder and removed with a conversation you `/forget`. Anything else, or on other channels, gets a quick reply saying it can't be opened there; it remembers you sent one, and a caption still comes through. A voice note that says "stop" stops what the twin is doing, like a typed one.
- **Finds another way to you.** When a reminder, an approval request or a task update can't be delivered where it belongs, it goes to your next connected channel, then to a desktop notification. You can answer an approval right where it reached you.

## Replies to other people

Every channel answers only you unless you set its `reply_to_others: true`. Then:

- The twin is told the sender isn't you, and sees none of your memory: no facts, no portrait, no `about`.
- Everything it would do for them, even looking something up, waits for you. You're asked in your own chat ("…on Telegram (not you) is asking me to…"), and you answer with `yes N` or `no N`; a bare "yes" never approves someone else's request. They hear the outcome.
- At most three people are answered at a time on each channel, so a crowd or a spammer can't run up your bill, and your own messages never wait behind them.

## Discord

1. https://discord.com/developers → New Application → Bot → Reset Token (put it in `DISCORD_BOT_TOKEN`). Under Privileged Gateway Intents turn on **Message Content**; without it the card says so instead of showing the bot as connected.
2. OAuth2 → URL Generator: scope `bot`, permissions Send Messages, Read Message History, Attach Files. Open the URL to invite it to a server you own (needed even for DMs).
3. Settings → Advanced → Developer Mode, right-click yourself → Copy User ID; set `channels.discord.owner` to it (or your username).
4. `channels.discord.enabled: true`, restart, DM the bot. In server channels it answers when @mentioned; list channel ids under `channels.discord.channels` to have it answer everything there.

## Slack

1. https://api.slack.com/apps → Create New App (from scratch). Socket Mode → enable, create an app-level token with `connections:write` → `SLACK_APP_TOKEN` (xapp-).
2. OAuth & Permissions → bot scopes: `chat:write`, `im:history`, `im:read`, `im:write`, `users:read`, `files:write`, `app_mentions:read`, `channels:history`. Install to workspace → `SLACK_BOT_TOKEN` (xoxb-).
3. Event Subscriptions → enable, subscribe to bot events `message.im` and `app_mention`. App Home → turn on the Messages tab and "Allow users to send Slash commands and messages".
4. Profile → ⋯ → Copy member ID → `channels.slack.owner`. Enable, restart, DM the app. In channels it answers when @mentioned.

## Signal

Signal has no bot API; the channel drives [signal-cli](https://github.com/AsamK/signal-cli) (`brew install signal-cli`, needs Java).

1. Either register a second number for the twin (`signal-cli -a +61400000001 register`, then `verify CODE`), or link it as a device of your own account (`signal-cli link -n mirrin` and scan the QR in Signal → Linked devices). A separate number is cleaner: then you message the twin like a contact.
2. `channels.signal.account` = the twin's number (or, linked to your own account, your own number), `owner` = your number, enable, restart. The daemon spawns `signal-cli … jsonRpc` itself; if you already run `signal-cli daemon --http`, set `channels.signal.http: http://127.0.0.1:8080` instead.

Linked to your own account (`account` and `owner` the same number), you talk to the twin in **Note to Self**; what you send other people is ignored. Group messages are ignored.

## Matrix

Unencrypted rooms only (no libolm). Works with matrix.org or your own Synapse/Conduit.

1. Make an account for the twin. Get a token: Element → Settings → Help & About → Access Token, or `curl -XPOST https://matrix.org/_matrix/client/v3/login -d '{"type":"m.login.password","user":"mirrin","password":"…"}'`.
2. `channels.matrix.homeserver`, `user_id` (`@mirrin:matrix.org`), `MATRIX_ACCESS_TOKEN`, `owner` (`@you:matrix.org`). Enable, restart.
3. The twin opens an unencrypted DM with you (accept the invite) and also accepts room invites from you. If you message it in an encrypted room it will say so and ask for an unencrypted one.

## Mattermost

System Console → Integrations → Bot Accounts → enable, then Integrations → Bot Accounts → Add. Copy the token to `MATTERMOST_TOKEN`, set `channels.mattermost.url` and your username as `owner`. DMs always; in channels it answers when @mentioned. Self-hosted friendly.

## IRC

```yaml
channels:
  irc:
    enabled: true
    server: irc.libera.chat:6697
    tls: true
    nick: mirrin
    password_env: IRC_PASSWORD     # the twin's own NickServ password (SASL PLAIN); optional
    owner: youraccount             # your NickServ account, or a full mask like yournick!~you@your.host (* and ? allowed)
    channels: ["#yourroom"]
```

Anyone can take a free nick, so a nick alone never makes someone you. You're recognised by your services (NickServ) account, which the server attaches to each message, and your conversation stays yours whatever nick you use. On a network without services, use a full `nick!user@host` mask, ideally with a cloak or a host you control. Old prefix masks such as `yournick!~you@` still work, and the Channels page warns when a mask would accept anyone. A message from your nick that isn't logged in to your account is ignored.

Private messages from the owner are answered; in joined channels, lines starting with `mirrin:` (the twin's nick) are. Replies are paced to stay under flood limits. Because an IRC sender can be faked, a `yes N` sent over IRC only answers requests raised in that IRC conversation; answer anything else on the presence screen.

## Zulip

Settings → Personal → Bots → Add a new bot (Generic). Set `channels.zulip.site` (`https://org.zulipchat.com`), the bot's email, `ZULIP_API_KEY`, and your email as `owner`. Direct messages always; stream messages when @-mentioned (replies land in the same topic).

## Email as a channel

Any mailbox becomes a two-way channel: you email the twin, it replies in the thread. Uses the mailbox under `skills.email` (so configure that first), polls every `poll_seconds` (30), and only answers mail from `owner` unless `reply_to_others`. Set `subject_tag: "[Mirrin]"` if that mailbox also receives other mail and you want only tagged threads answered. Slow, but it works from every phone, car and watch on earth.

A From address is whatever the sender typed, so mail counts as yours only when your provider's own check passed for you: an SPF, DKIM or DMARC pass in the `Authentication-Results` header its receiving server adds. Gmail, Fastmail, iCloud, Yahoo, Zoho and Microsoft are recognised from `imap_host`; for another provider the IMAP host's own domain is the guess. If mail from you is ignored, or you use a local bridge such as Proton Mail Bridge, set `skills.email.auth_servers` to the server named at the start of that header (open a message from yourself and look at its headers); list every server that writes one, and a parent domain covers the servers under it. The Channels page says when mail in your name was ignored, and why. Use an address other than the twin's own mailbox as `owner`: mail it sends itself may carry no such header.

Only mail that arrives while the channel runs is read, and only mail the twin answers is marked read; everything else in the mailbox stays unread for you. As with IRC, a `yes N` by email only answers requests raised in that email conversation; answer anything else on the presence screen.

## Browser (the hands)

`skills.browser` needs Chrome or Chromium on the machine. It runs one real Chrome that stays open between calls and keeps its own profile under `~/.mirrin/data/chrome-profile`, so anything you log into stays logged in. Every page comes back as text, a numbered list of clickable elements, and a screenshot the model actually looks at, so it works on sites with no API: banks, bookings, government portals, shops.

When a site needs you (a password, a code, a CAPTCHA), the twin calls `browser_signin` and hands you the page on the presence screen: click and type on it there, say "done", and it carries on. If the screen isn't working for you, say so and it opens a Chrome window on your computer instead (`handover: window` always does). Google's sign-in turns away a browser that something drives, so for a chat at your computer a Google page (Gmail, Drive, the sign-in itself) always opens in ordinary Chrome, with the twin's own profile and nothing driving it; when you say "done" the twin closes that window and carries on signed in. A window stays up 20 minutes. Set `headless: false` if you'd rather always watch it work. Anything irreversible still goes through approvals, and the approval message carries the screenshot of the page as it is.

```yaml
skills:
  browser:
    enabled: true
    headless: true
    idle_minutes: 10
```

## Google account (Calendar, Gmail, Drive)

Menu bar → **Accounts…** → Google. One sign-in covers Calendar (read and write), Gmail (search, read, send, reply, archive) and Drive (search and read Docs, Sheets, Slides and text files). The first time on a computer you need an OAuth client so Google knows the app: Cloud console → Credentials → OAuth client ID → Desktop app, enable the Calendar, Gmail and Drive APIs, then paste the client id and secret into the card (or drop the JSON). Publish the app on its Audience page before you connect: while it is in Testing, Google ends every sign-in after 7 days (the twin tells you when it does, with the fix). Press Connect on the computer the twin runs on (Google returns the sign-in there, whatever address the twin also listens on), approve in the browser, and you land back on the page with the three services switched on. Each can be toggled; Disconnect forgets the token. `mirrin calendar login` is the terminal route to the same sign-in: save the Desktop-app client JSON as `~/.mirrin/google-credentials.json` first. The sign-in is kept in `~/.mirrin/google-token.json`, readable only by you.

Other services connect as MCP servers (`mcp.servers`) or as custom tools the twin writes.

## Phone (Twilio)

```yaml
phone:
  enabled: true
  account_sid: ACxxxxxxxx
  auth_token_env: TWILIO_AUTH_TOKEN     # or auth_token:
  from: "+61480000000"                  # your Twilio number
  voice: Polly.Olivia-Neural            # Polly.Brian-Neural (UK), Polly.Joanna-Neural (US)
  language: en-AU
  public_url: https://mirrin.tail1234.ts.net   # optional: makes calls two-way
```

`send_sms` texts from the twin's number. `phone_call` rings someone and speaks an opening line on your behalf; every call needs your approval. With a public address, the call becomes a conversation: `public_url` (a Tailscale funnel or ngrok pointing at the API port), or, when that is empty, the address your own relay serves (`mirrin reach use relay`, [reach.md](reach.md)). Then Twilio sends what the other party says to `/phone/turn`, the model answers in one or two sentences, and so on until it says goodbye. Requests are checked against Twilio's signature. When the call ends you're told how it went, including the last thing said, and `call_transcript` shows the whole of it for a day. Anything the twin needs your yes for during a call is asked in your own chat, never on the line.

## Teach mode

Say "let me show you" and name the routine. A window opens; do the task once the way you normally would. The twin records the clicks, the typing (never passwords) and the pages, then reads the steps back and offers to save them as a protocol, on a schedule if you like. Next time it replays them itself, handing you the window only for anything secret.

## Spending limits

```yaml
spending:
  currency: AUD
  per_action_limit: 100   # a single payment above this needs your explicit yes to that amount
  monthly_limit: 500      # once reached, no more payments until you raise it
```

Before any step that pays, the twin calls `check_spend`; after a payment goes through it records it. A click on anything that looks like Pay, Buy, Checkout or Transfer counts as dangerous, so it is sent to you for approval with the page screenshot even if `always_allow` lists the tool (only `autonomy.dangerous: auto` would skip that; keep it on `ask`). `/spend` shows this month's ledger. Card numbers and passwords are never typed by the twin: it hands you the window for those.

## Background tasks

Anything with several steps or waiting in it ("plan the Bali trip", "chase the refund", "find three plumbers and get quotes") runs as a background task. The twin puts a plan on a board, works through it with a large tool budget, and messages you only when it needs a decision, an approval or a login, or when it is done. Reply in the same chat and the answer goes to the task. `/tasks` shows the boards; the presence screen has a "Working on" panel. Tasks are saved and resume after a restart; finished ones are kept a week. Cancelling stops the work in progress. A task that runs out of steps pauses and tells you what's left rather than claiming it's done, and one that fails, or works for 30 minutes straight without finishing, tells you how to retry (the clock restarts after each pause for you).

## Custom tools (the twin extends itself)

Anything a short script can do becomes a tool. Say "write yourself a tool that…" and the twin drafts it with `create_tool`; the approval shows you the whole script, it smoke-tests it, and from then on the tool is there. Tools live under `~/.mirrin/tools/<name>/` as a `tool.yaml` manifest plus `tool.py`, `tool.js` or `tool.sh`; write your own the same way and they load at start. The script gets the arguments as `ARG_<NAME>` environment variables and the JSON input on stdin, prints its result, and exits non-zero to fail.

Mark a tool `risk: read` if it only looks things up; `write` and `dangerous` go through approvals like everything else. A tool the twin wrote itself never counts as less than `write`, whatever it claims, so it asks before every run; add its name to `autonomy.always_allow` once you trust it. One it marked `dangerous` stays dangerous, and `always_allow` never covers that. A tool you wrote keeps the risk you gave it.

Scripts start with a minimal environment (`PATH`, `HOME`, locale, temp and proxy settings, and where language runtimes live, such as `JAVA_HOME` and `PYTHONPATH`), never Mirrin's own keys and tokens. List the variables a tool needs under `env:`; each is taken from your environment, or from `~/.mirrin/secrets.env`. Keep secrets there, never in the script. The background service doesn't see your shell, so `mirrin service install` copies the variables your tools name under `env:` from your shell into `~/.mirrin/secrets.env` (readable only by you), along with the twin's own keys. It copies only what is exported in the shell at install time, so for a tool written later, or a key you hadn't exported then, run `mirrin service install` again, or add a `NAME=value` line to `~/.mirrin/secrets.env` yourself (keep it 0600).

```yaml
name: parcel_status
description: Where an Australia Post parcel is right now.
risk: read
command: python3 tool.py
env: [AUSPOST_API_KEY]
args:
  id: {type: string, description: Tracking id, required: true}
```

## MCP servers

Any stdio MCP server works, and its tools appear to the twin under the approvals gate, named `server__tool`. Config lives under `mcp.servers`:

```yaml
mcp:
  servers:
    - name: files
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/Documents"]
      risk: read                          # default for the server's tools
      tool_risk: {write_file: write, move_file: write}
    - name: github
      command: npx
      args: ["-y", "@modelcontextprotocol/server-github"]
      env: {GITHUB_PERSONAL_ACCESS_TOKEN: "$GITHUB_TOKEN"}   # $NAME passes your own variable through
      risk: write
```
 Risk levels decide whether a tool runs unasked: `read` runs, `write` asks, `dangerous` asks (or is blocked by the autonomy policy). Set the server's default with `risk` and override per tool with `tool_risk`. Servers that fail to start are skipped with a warning; Mirrin still comes up. Tested with `@modelcontextprotocol/server-filesystem`.

A server starts with the same minimal environment as a custom tool, plus its `env` block. Write `$NAME` (or `${NAME}`) as a value to pass one of your variables through without putting it in `config.yaml`:

```yaml
env: {GITHUB_PERSONAL_ACCESS_TOKEN: "$GITHUB_TOKEN"}
```

`$GITHUB_TOKEN` comes from your environment, or from `~/.mirrin/secrets.env`; `mirrin service install` saves it there for the background service. It travels with `mirrin identity export` as the name, never the value.

## Model providers

```yaml
llm:
  provider: ollama              # anthropic | openai | gemini | ollama | openai-compatible
  model: gpt-oss:20b
  providers:
    anthropic: {api_key_env: ANTHROPIC_API_KEY, model: claude-opus-5}
    openai:    {api_key_env: OPENAI_API_KEY, model: gpt-4.1}
    gemini:    {api_key_env: GEMINI_API_KEY, model: gemini-2.5-pro}
    ollama:    {base_url: "http://127.0.0.1:11434/v1", model: gpt-oss:20b}
```

`llm.provider` picks the backend; `llm.providers.<name>` holds each one's key (or `api_key_env`), `base_url` and last model, so switching from the menu bar keeps your choices. `openai-compatible` with a `base_url` covers LM Studio, vLLM, LiteLLM and friends. Ollama needs no key and lists installed models; `mirrin models [provider]` lists what each offers. Tool calling works on all of them; quality varies with the model, and small local models will make more mistakes than Claude.

## Quick judgments (TypeSafe Jev, optional)

Off by default, and nothing changes without it. With a key from [TypeSafe](https://typesafe.ai), the twin can ask Jev, a small model that returns a typed judgment (a yes probability, or one option from a list, with how sure it is) instead of text. It is only asked after the twin's own exact rules didn't settle something, and when it is off, fails, times out or isn't sure, the twin does exactly what it does without it.

Each use sends only the few words its question needs, and never your memories, facts or other mail:

- **Bills:** with bill watching on, when mail that looks like a bill arrives, its sender and subject line (up to ten emails in one question per check), so receipts, statements with nothing to pay and adverts don't take a full model run. Jev only ever skips that run, and only when it is very sure; anything else is read as before.
- **Replies to reminders:** your short typed reply ("paid it", "not yet", "remind me later") and the reminder it answers, so it can tick or move the reminder. Photos, voice notes, longer messages and replies that also ask for something go to the model as before.
- **Replies to the twin's notes:** your short reply ("sure go ahead", "stop sending these") and the offer it answers, with names left out, or just the kind of note (the Sunday note, a meeting brief), never its text.

It is never used to decide an approval.

To turn it on, open the Accounts page, paste your key under **Quick judgments (optional)**, and press **Check and save**: the key is tested with one fixed question, saved in `secrets.env` and never shown again in full. **Remove key** forgets it and switches it off. Or by hand:

```yaml
jev:
  enabled: true      # a key alone never turns it on
  # api_key_env: TYPESAFE_API_KEY   (the default; keep the key in secrets.env)
  # model: jev-latest
```

Each call has two seconds, including one retry when TypeSafe is busy. Usage goes in the usage ledger as `jev.<use>`; there is no built-in price, so add one under `usage.prices` if you want it counted. A key TypeSafe refuses shows on the card.

## Multi-device

Set `api.remote: true` and `api.listen` to the home machine's Tailscale address (`100.x.y.z:7742`; `0.0.0.0:7742` works too, but listens on every network), and restart. Then, on the home machine, `mirrin pair` makes a single-use code for another computer, `mirrin pair --screen` a link and QR code for a phone or tablet, and `mirrin pair --kiosk` a view-only link for a wall screen. A code or link works once, for 10 minutes, and never holds the home machine's master key: each device gets a key of its own, with only the scopes it was paired for (`--scopes view,chat,approve,admin`; settings answer on the home machine only). You're told when a device is paired, with how to cut it off; `mirrin devices` lists, renames and revokes them (the twin needn't be running), and `/revoke` in your own chat does the same. `mirrin connect <code>` on another computer verifies the connection and saves it; `chat` and `voice` there become thin clients, and `mirrin disconnect` goes back to the local twin. Reminders set from a remote session are delivered on the home machine's channels. `api.remote` is plain HTTP, so use it over [Tailscale](https://tailscale.com), where traffic is already encrypted, or a home network you trust, and never forward the port to the internet. For HTTPS instead (and the phone app, lock-screen approvals and Face ID), use `mirrin reach use tailscale`, your own certificate or your own relay: [reach.md](reach.md).

## Change watcher

`watch.enabled` (default on) snapshots the calendar (next 7 days) and new mail every `interval_minutes` (5) and asks the agent about differences. New mail means the unread IMAP inbox under `skills.email`, or Gmail's Primary inbox once the Google account is connected (when both are the same Gmail, it's watched once). It only runs for skills that are enabled, and it follows the Accounts page as you go: connecting Google or switching Calendar or Gmail on starts watching it at once, from what's there now, and disconnecting or switching off stops it, without a restart. What senders and invites wrote is treated as information, never instructions, and the watcher sees none of your memory; anything it would look up or do, even a read, waits for your yes. Turn a source off with `watch.calendar: false` or `watch.inbox: false`.

## Meeting briefs

With Google Calendar connected, about ten minutes before a meeting the twin looks at who is coming. If it knows something useful about one of them, from your memory or from the last month of mail with them (when Gmail is connected), you get one line in your own chat and on the presence screen: "Priya at 11. Last time you promised her the Q3 numbers." A meeting gets at most one brief. A routine meeting with no one it knows anything about gets nothing, and the model isn't even asked; nor does a meeting you declined, one with no one else, or one with more than twelve guests. Facts about health, money, relationships and secrets are never used, and a line that strays into them isn't said. The brief only reads (memory, `gmail_search`, `gmail_read`), treats mail and invites as information, never instructions, and goes to you alone. Nothing is said in quiet hours. On your computer's notifications it only says a note is waiting, and it is never read out loud while you're in another meeting. Turn briefs off by saying "no more meeting briefs", or with `watch.meeting_briefs: false`.

## Email

Works with any IMAP/SMTP provider. On Gmail, the Google account (above) is simpler: one sign-in, no password to keep, and the watcher sees your Primary inbox. Use IMAP for Gmail only if you'd rather not make an OAuth client.

```yaml
skills:
  email:
    enabled: true
    imap_host: imap.gmail.com
    imap_port: 993
    smtp_host: smtp.gmail.com
    smtp_port: 587
    username: you@gmail.com
    password_env: MIRRIN_EMAIL_PASSWORD
    from_name: Your Name
    # auth_servers: [mx.example.org]   # only if the email channel ignores mail from you (see Email as a channel)
```

Gmail over IMAP: create an App Password (Google Account → Security → 2-Step Verification → App passwords). Export it as `MIRRIN_EMAIL_PASSWORD` before `mirrin service install`, which saves it in `~/.mirrin/secrets.env` (readable only by you) for the background service.

## System

`allowed_dirs` fences file access (default: your home). Some places always count as dangerous, so reading, listing or writing them asks first and the approval says why: SSH and GPG keys, cloud credentials (`~/.aws`, `~/.azure`, `~/.config/gcloud`, `~/.kube`, `~/.docker`), keychains and password stores, browser profiles, `.env` files, private keys, and Mirrin's own home and data folder.

`run_shell` is `dangerous` too, so it asks every time and the approval shows the whole command and folder. `always_allow` never covers a dangerous action; only `autonomy.dangerous: auto` would, and we don't recommend it. To take the shell away altogether, set `skills.system.allow_shell: false`.

## Web

`fetch_url` reads a web page as text. It stays on the public internet: this machine, your home network, Tailscale addresses and cloud metadata services are refused, checked on every connection and redirect, and through a proxy (`HTTPS_PROXY`) as well. The browser keeps to the same rule, for pages, redirects, images and WebSockets, and uses the same list. To let either reach something at home, list it (a change from the settings applies to both at once):

```yaml
skills:
  web:
    enabled: true
    allow_hosts: [homeassistant.local, 192.168.1.20, 10.0.0.0/8]   # names, addresses or ranges
```

## Writing a skill

```go
package weather

func Tools() []tools.Tool {
    return []tools.Tool{
        tools.New("weather", "Current weather for a city.",
            tools.Schema(map[string]tools.Prop{"city": {Type: "string", Required: true}}),
            tools.RiskRead,
            func(ctx context.Context, call tools.Call) (string, error) {
                var in struct{ City string }
                if err := tools.Decode(call, &in); err != nil { return "", err }
                return lookup(ctx, in.City)
            }),
    }
}
```

Register it in `internal/daemon/daemon.go` and open a PR.

## Chores

Three protocols ship for the most common "please just handle it" requests. They orchestrate existing tools, so they work as far as those connections do:

- **reply like me** — `list_emails`, `read_email`, then `list_sent_emails` / `read_sent_email` to learn your voice, then `reply_email` (threaded, approval-gated).
- **rebook** — `list_events`, `update_event` / `delete_event`, a message to the other party, and `browser_inspect` / `browser_act` when the booking lives on a website.
- **chase refund** — finds the receipt, drafts the request or fills the merchant's form with `browser_act`, then `set_reminder` for a five-day follow-up.

`browser_act` takes a JSON list of steps (type, click, select, wait, submit) and returns the page text plus a screenshot path. When Mirrin asks for approval and mentions that screenshot, WhatsApp and Telegram receive the image with the request. These flows were exercised against local test pages and the tool mechanics, not against live merchant sites; expect to tune selectors per site.

## Health

`mirrin doctor` (or the Health item in the menu, or `/health`) runs the self-checks: the model really answers (a bad key, a model Ollama hasn't pulled), API up, disk, whisper/sox/model present, speaking engine, listening channel alive, messaging channels connected, Google (signed in, still accepted, APIs on), mailbox password, backups, model spending. It says whether Mirrin is running, and exits 1 when something is wrong, so scripts can tell. Checks run at startup and hourly; a check that fails and has a repair (restarting the listener) is repaired automatically, and a change in status posts a desktop notification.

## Memory portrait

Every Sunday at 8, Mirrin writes a short portrait of you from what it knows and keeps it in memory; it appears on the memory page and in its own prompt. Refresh it any time from the page. The first one is written at the end of the first week, and the last tip of the week says so. On the presence screen, "How Mirrin sees you" shows it in full with a line on what's new this week, and asks whether it's right: "That's you" puts the question away until the next portrait, and "Not quite" opens the text box for you to say what's off. It is never read aloud or sent to a chat, and a wall screen paired only to look never shows it. Facts in the health, finance, relationships or secrets categories go through `remember_sensitive`, which needs your approval.

Every turn sees your facts: all of them while they fit, past that the ones related to what you said and the newest, and the twin knows to `recall` the rest. `forget` removes a fact and every copy the twin kept of it (the call that stored it, its approval, the activity log, recall results, its own word-for-word repeats, deleted space on disk and the daily copies); your own messages are left alone. Encrypted backups already written keep a forgotten fact until retention removes them, and the next one is taken soon after; to be rid of it sooner, take a new backup and prune (docs/backup-format.md, "Forgetting and backups"). `memory.db` is copied to `~/.mirrin/data/backups` once a day after a health check, and the last seven are kept.

## Presence and discretion

- **Acknowledgement.** On wake a soft chime plays (replace it with any file via `channels.voice.chime_sound`, e.g. `/System/Library/Sounds/Tink.aiff`). The moment you stop talking it says a short word: "Hello." / "Yes?" / "I'm here." for the first exchange after ten quiet minutes, otherwise "Right." / "On it." / "Let me see." Clips come from the persona's `acks` and are synthesised once with the active voice. Turn off with `channels.voice.acknowledge: false`.
- **Approving out loud.** A spoken yes approves writes as usual, but never a dangerous request (a payment, a shell command, a call): anyone in the room can say "yes". The twin sends that request to your own chat with its number and says where; approve it there with `yes N`, or on the presence screen. A no is always taken.
- **Conversation.** After each reply it listens for a follow-up (`followup_seconds`, default 8) without the wake word, and at least 12 seconds when it has just asked you something. Speech in that window is always treated as your reply, even if the wake model twitches on it. While it is still talking, **saying its name always interrupts it**. Simply talking over it can too: the helper compares the microphone level with what its own playback measures at the mic and treats sustained speech clearly above that as an interruption, then whisper confirms it was real speech (and not the twin's own echo) before the reply is cut. On a laptop the microphone sits next to the speakers, so this is a judgement call: `talk_over: off | low | normal | high` (menu: Voice → Interrupt by talking over it). `normal` never interrupts itself but needs you to speak up; `high` is for a headset or a close microphone; `off` leaves interruption to the name.

Microphone level matters more than any setting. If replies are missed, raise the input volume in System Settings → Sound → Input to around 70; the speech detector adapts to the room's noise floor but cannot invent signal.
- **Narration.** Every slow tool call gets a spoken caption ("Reading your email.") on voice, an indented note in the terminal, and a `note` event over the streaming API. Instant tools (reminders, memory) are silent.
- **Budgets.** Tool calls per request are capped by the size of the ask: check-in 1, question 3, instruction 6, protocol 25, each leg of a background task 60. When the budget runs out the model says what it got done and what's left, and offers to carry on; it never claims "Done." for work it didn't finish. Override in code via `agent.Budgets`.
- **Patterns.** Daily at 5pm Mirrin clusters two weeks of your requests by shared words; a theme asked three or more times gets one offer of a protocol with a schedule. Say yes and it drafts one with `create_protocol`; you approve the schedule and instructions, and it's saved and scheduled. Each theme is offered once.

## The screen on a wall

1. On the home machine: `mirrin reach use tailscale` (HTTPS on your tailnet; [reach.md](reach.md)), restart. Run `mirrin pair --kiosk` (view only; use `--screen` if the wall should Approve/Deny too) for a single-use link.
2. On a Raspberry Pi (Raspberry Pi OS with desktop): install Tailscale and join the same tailnet, then autostart Chromium in kiosk mode on the home machine's `ts.net` name (Linux listens on port 7743, macOS and Windows on 443; `mirrin reach status` shows the name):
   ```
   chromium-browser --kiosk --noerrdialogs --disable-infobars --incognito=false "https://<home-machine>.<tailnet>.ts.net/ui"
   ```
   Use the exact address `mirrin reach status` prints: on a Linux home machine it ends in `:7743` (`https://<home-machine>.<tailnet>.ts.net:7743/ui`).
   Open the `https://…/pair#…` link `mirrin pair` printed once in that browser first: pairing sets the screen's own cookie (180 days from the last visit), and later starts use `/ui` as above. Disable screen blanking (`xset s off; xset -dpms`). The older plain-HTTP way (`api.remote: true` with `api.listen` on the Tailscale address, and `http://<ip>:7742/`) still works.
3. Optional: `ui.latitude` / `ui.longitude` for weather somewhere other than the home machine's time zone city (weather is on by default and asks Open-Meteo with that city's coordinates; `ui.weather: false` turns it off), `ui.ambient_after_seconds` for how quickly it goes ambient (default 60).

The screen sends back approve or deny on a card, and whatever you type once you start typing (press any key, or tap the orb). On a phone (a touch screen up to 900px wide, any window up to 600px, or a phone held sideways) the text box is always at the bottom. A screen paired with `--kiosk` shows neither. Everything else is voice.
