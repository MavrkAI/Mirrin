# Protocol reference

This page is for anyone who writes, checks or debugs protocols: it lists every field, rule, message and command, exactly as Mirrin has them, so you can look up what a file will do before your twin runs it.

New to protocols? Start with the [quickstart](quickstart.md). For how to write a good one, see the [writing guide](writing-guide.md).

## Where protocols live

Your protocols are YAML files in your protocols folder. That is `protocols_dir` in `~/.mirrin/config.yaml`, which defaults to `~/.mirrin/protocols` (or `protocols` under `MIRRIN_HOME`, when that is set).

- **What is read.** Every `.yaml` or `.yml` file directly in the folder, whatever the case of the extension. Subfolders are not searched.
- **What is left alone.** Folders, files whose names start with `.`, `vars.yaml` (your values, see [Vars](#vars)) and `pack.yaml`.
- **Packs.** Installed packs live in `packs/` inside the protocols folder, one folder each, and their protocols load next to yours. How packs are laid out, installed and updated is in [packs.md](packs.md).
- **New files.** `mirrin protocols new` and the twin's `create_protocol` name a new file after the protocol: lowercase letters and digits in any script, with every other run of characters turned into one hyphen. `Rain Check!` becomes `rain-check.yaml`, and `朝のまとめ` stays `朝のまとめ.yaml`. They never replace a file that is already there, and the file is readable only by you.
- **A fresh install** starts with six starter protocols, written into the protocols folder only while it is empty. They are the files in [`examples/protocols`](../../examples/protocols).

Nothing watches the folder. After you edit a file by hand, say `/reload` to your twin (see [Reloading](#reloading)).

## The fields

A protocol is one YAML mapping. Only `prompt` is needed for the file to load.

| Field | Type | Default | Required | What it does |
|---|---|---|---|---|
| `name` | text | the file name without its extension | lint says yes; the loader falls back to the file name | What people say to run it, and the key for its values in `vars.yaml`. Matched ignoring case everywhere except `vars.yaml`. Keep it lowercase. |
| `description` | text | empty | no; lint warns | One line, shown by `mirrin protocols list`, in your twin's list and its approval to run it, in a pack preview in chat, and on the Routines page. |
| `version` | text | empty | no; lint warns | Shown on the Routines page. Use semver (`0.1.0`). Nothing compares versions. |
| `author` | text | empty | no; lint warns | Your name or handle. |
| `tags` | list of text | none | no | Shown on the Routines page. The tag `briefing` also changes how a run behaves (see [Briefings](#briefings)). Registry search matches a pack's tags, not these. |
| `schedule` | text | empty, so it runs only when asked | no | A five-field cron expression in your twin's time zone (see [Schedules](#schedules)). |
| `requires` | list of text | none | no | The tools or skills the prompt relies on (see [Requires](#requires)). Reported when missing; never stops a run. |
| `vars` | map of names to vars | none | no | Inputs the person running it fills in, used as `{{name}}` in the prompt (see [Vars](#vars)). |
| `prompt` | text | none | yes | The brief your twin follows on each run. A file with no prompt, or a blank one, is skipped. |
| `enabled` | `true` or `false` | `true` | no | `false` takes it off the schedule. It can still be run when asked. |

Each entry under `vars` has three fields:

| Field | Type | Default | What it does |
|---|---|---|---|
| `description` | text | empty | Says what to fill in. It is quoted in the message a run sends when the value is missing, so write it as a short noun phrase: "The city to check the forecast for". |
| `default` | text | empty | Used when `vars.yaml` has no value for it. |
| `required` | `true` or `false` | `false` | Documents that the person must fill it in. It has no effect at run time: any var with no value and no default stops the run (see [Vars](#vars)). Lint warns if a var is both required and has a default. |

Two things about YAML to know:

- **Unknown keys are ignored without a word.** A typo such as `scheudle:` is dropped, and the protocol runs only when asked. Lint doesn't catch it, so read your keys carefully.
- **Values are text.** A number or `true` in a `default` or in `vars.yaml` is read as the text `5` or `true`.

## Names

- A protocol's name is what people say: "run rain check". Your twin finds it by exact name, ignoring case.
- Two of your own files with the same name (ignoring case): the one whose file name sorts later is skipped, with a message (see [The loader](#the-loader)).
- Your file and a pack's file with the same name: **yours wins, silently.** This is how you change one protocol from a pack without forking the pack.
- Two packs with the same protocol name: the pack whose folder name sorts later loses that protocol, with a message.
- Changing a pack protocol's schedule, or turning it off or on, from chat or the Routines page writes a copy into your own folder, in a file named after it as for a new file (`rain-check.yaml`, or `rain-check-2.yaml` up to `-9` if that is taken), with `name:` set. That copy wins over the pack's, so it **no longer follows the pack's updates**. The approval and the page both say so.
- Changes to your own files from chat or the page keep the rest of the file, comments included, and are written in one go.

## Schedules

A schedule is a cron expression with five fields, separated by spaces:

| Field | Allowed values |
|---|---|
| minute | 0 to 59 |
| hour | 0 to 23 |
| day of month | 1 to 31 |
| month | 1 to 12, or `jan` to `dec` |
| day of week | 0 to 6 with Sunday as 0, or `sun` to `sat`. `7` is refused. |

Each field takes `*` (any), a number, a list (`1,15`), a range (`1-5`, `mon-fri`) or a step (`*/30`). If you restrict both the day of month and the day of week, it runs on days that match either, as in standard cron.

Your twin and the Routines page say schedules in words. These shapes are put in words; anything else is shown as the cron text:

| Schedule | In words |
|---|---|
| `0 7 * * *` | every day at 7:00 |
| `30 6 * * 1-5` | weekdays at 6:30 |
| `0 9 * * 6,0` | weekends at 9:00 |
| `0 17 * * 5` | Fridays at 17:00 |
| `0 8 * * 1,4` | Mondays and Thursdays at 8:00 |
| `0 8,18 * * *` | every day at 8:00 and 18:00 |
| `0 9 1,15 * *` | on the 1st and 15th at 9:00 |
| `0 * * * *` | every hour |
| `*/30 * * * *` | every 30 minutes |
| empty | when you ask |

The Routines page shows the times in your screen's own clock format.

**Time zone.** Schedules run in the zone your twin keeps: `user.timezone` in your config when you set one, otherwise the computer's zone, read again as it changes, so a laptop that travels follows it.

**Other forms.** The cron library also accepts shorthands such as `@daily` or `@every 2h`, and a `CRON_TZ=` prefix. They run, but avoid them: the Routines page can only show and change five-field times, and a zone prefix ignores the zone your twin keeps. Write five fields.

**A schedule that can't run.**

- Created in chat or saved by the twin: refused before anything is written, with a plain explanation (see [Saving](#saving)).
- Written by hand: `mirrin protocols lint` reports it as an error. At load, that one protocol isn't scheduled and every other protocol carries on. It is written to [the log](#the-log), and the `/reload` reply names it. You get no message about it, and `mirrin protocols check` doesn't look at schedules, so run `lint`.

**When runs happen.** The scheduler looks at the clock at least every 20 seconds.

- Up to 10 minutes late: it runs as normal.
- From 10 minutes to 12 hours late (the computer was asleep, or the twin wasn't running): it runs once, however many runs were missed. Your twin is told the run is late, and can say nothing if it no longer makes sense. What it sends starts with a note such as `(Late — this was due at 06:30.)`.
- More than 12 hours late: it doesn't run. You get one message listing what was missed, ending `Say "run rain check" if you still want it.`
- While your twin is paused nothing runs. When you resume, one message lists what didn't run.
- **Skip next** (on the Routines page, or "skip tomorrow's rain check" in chat) skips exactly one scheduled run.
- Runs that fell due while the twin was off are made up when it starts again, under the same rules.

## Vars

Vars let one protocol fit many people. Declare them under `vars`, use them in the prompt as `{{name}}`, and each person fills in their values.

- **Syntax.** `{{city}}` or `{{ city }}`. Var names use ASCII letters, digits and `_` only. Anything else in braces, such as `{{my-city}}` or `{{städte}}`, is left in the prompt as written, and lint doesn't flag it.
- **Where values go.** One file, `vars.yaml` in your protocols folder (`~/.mirrin/protocols/vars.yaml`), with one block per protocol. The key is the protocol's `name`, **exactly, including case**:

  ```yaml
  rain check:
    city: Melbourne
  commute check:
    origin: 12 Example St, Brunswick
  ```

- **When they are read.** Values are read when protocols load: after editing `vars.yaml`, say `/reload` (see [Reloading](#reloading)). Until then your twin runs with the values it had. `mirrin protocols check` reads the file afresh, so it shows your edit straight away.
- **Order.** A non-empty value in `vars.yaml` wins, then a non-empty `default`. An empty value counts as no value. Each `{{name}}` is replaced once, as plain text.
- **Packs.** The same file holds values for pack protocols. A pack can't bring its own values: a `vars.yaml` inside a pack is ignored, so a pack sets sensible `default`s instead.
- **A broken `vars.yaml`** is reported like a skipped file, as `not valid YAML, so no vars were applied`. For a mistake in the YAML itself, such as a bad indent, that is so: no values apply to any protocol. A block of the wrong shape (a list where the values should be) is reported the same way, but the blocks that are fine still apply.
- **A var with no value and no default stops the run**, whether or not it is marked `required`:
  - A scheduled run is skipped and you are told, at most once a day: `Rain check didn't run: it still needs city (the city to check the forecast for). Add it under "rain check" in ~/.mirrin/protocols/vars.yaml, then say /reload.` The next run after that goes ahead.
  - Run now tells you the same every time.
  - In chat, `run_protocol` refuses with `Rain check can't run yet: it still needs …, say /reload, then try again`.
  - `mirrin protocols check` prints `! rain check  set vars in …/vars.yaml: city`.

## Requires

`requires` lists what the prompt relies on, so people know before they install it and your twin can say what's missing. Each entry is a tool name, such as `fetch_url`, or one of ten skill names. A skill is met when **any one** of its tools is there:

| Skill | Met by |
|---|---|
| `browser` | `browse_page` or `browser_inspect` |
| `calendar` | `list_events` |
| `drive` | `drive_search` |
| `email` | `list_emails` (IMAP) or `gmail_search` (Gmail) |
| `memory` | `remember` |
| `phone` | `phone_call` or `send_sms` |
| `protocols` | `list_protocols` or `run_protocol` |
| `reminders` | `set_reminder` |
| `system` | `read_file` |
| `web` | `fetch_url` |

Which tools a twin has depends on what is set up: `list_events` appears once the calendar is on, Gmail and Drive tools need a connected Google account, and tools from an MCP server are named `<server>__<tool>`. Setup is in [docs/skills.md](../skills.md).

**Prefer skill names.** They are known on every machine. A tool name is checked against the tools of the twin running lint, so it warns on a machine where that tool isn't connected.

**Missing requirements are reported, never enforced.** A protocol whose requirements aren't met is still scheduled and still runs. You see what's missing in:

- `mirrin protocols check`: `✗ rain check  needs: calendar`;
- the Routines page: "needs: calendar";
- [the log](#the-log), when protocols load.

A pack preview, in chat or on the page, lists what each protocol requires before you install it.

## Running

A run starts on schedule, from **Run now** on the Routines page, from **Run protocol** in the menu bar, or when you ask your twin in chat or out loud (`run_protocol`). The menu bar's list is made when the menu bar app starts, so a protocol added since then appears there after the app restarts.

**What your twin is given.** The prompt, with vars filled in, framed so the model knows nobody typed it:

```
[Scheduled task from your heartbeat, not typed by the user. Do the task and reply with what the user should see. If nothing needs saying, reply exactly: NOTHING_TO_REPORT]

Protocol "rain check": Look at today's calendar with list_events ...
```

- Each run has a conversation of its own, so it never mixes with a live chat or with another run. It doesn't carry on from the last run.
- A run can make up to 25 tool calls.
- A scheduled run, Run now or the menu bar has 10 minutes; a run asked for in chat has 5.

**`NOTHING_TO_REPORT`.** If a scheduled run or Run now replies with anything containing `NOTHING_TO_REPORT`, nothing is sent. Lint warns when a scheduled prompt doesn't mention it, and the twin's `create_protocol` adds `If there is nothing worth saying, reply NOTHING_TO_REPORT.` to a scheduled prompt that lacks it. A run asked for in chat is different: its result goes back into that conversation, and your twin tells you what it found, quiet or not.

**Where the result goes.** To the first messaging app that has your own chat, in this order: WhatsApp, Telegram, iMessage, Signal, Discord, Slack, Matrix, Mattermost, Zulip, IRC, email. With none of those, it goes to the presence screen with one desktop notification, `Rain check is ready.`

- On the presence screen it is also read aloud, but only to someone in the room: when voice is on, outside quiet hours, and you have talked to your twin out loud in the last ten minutes.
- Quiet hours don't hold a protocol's result. A routine scheduled for 23:30 messages you at 23:30.

**When nothing runs.**

- While your twin is paused, nothing runs, Run now included (the page still says "Started").
- Once this month's model budget (`usage.monthly_budget`) is used up, background runs stay quiet. You are told once that month.
- Saying "stop" ends a run quietly.
- `enabled: false` keeps a protocol off the schedule. Run now and chat still run it.

### Briefings

The tag `briefing` is the one tag that changes behaviour:

- What your twin held back during quiet hours (the watcher's news, ideas, tips, a background task's result) is handed to the next briefing run to mention briefly.
- Held notes wait for a briefing that is due within 45 minutes, rather than going out on their own.
- On the presence screen a briefing stays open under "Left for you" until noon.

Tag only a protocol that sums up the day, as the starters `morning briefing` and `evening wrap` do.

### When a run fails

You get a plain message, `Rain check didn't run this time: <reason>.`, and for a scheduled protocol ` I'll try again next time it's due.` There is no retry before the next due time.

<!-- messages:start -->

| Message | When |
|---|---|
| `<Name> didn't run this time: <reason>.` | A run failed. |
| `I'll try again next time it's due.` | Added for a scheduled protocol. |
| `it took longer than ten minutes, so I stopped it` | The reason, when a run hit its limit. |
| `it was interrupted` | The reason, when a run was cancelled. |
| `the model service didn't accept the API key` | The reason, for a 401 or 403 from the model. |
| `the model service asked me to slow down` | The reason, for a 429. |
| `the model service was busy or down` | The reason, for a 5xx. |
| `something went wrong on my side (the details are in the log)` | Any other reason. A missing key or a model problem is put in plain words instead. The details are in [the log](#the-log). |
| `<Name> didn't run: it still needs <vars>. Add it under "<name>" in <vars.yaml>, then say /reload.` | A var has no value (scheduled run or Run now). |
| `<Name> is ready.` | The desktop notification for a result that went to the presence screen. |
| `(Late — this was due <when>.)` | Starts a late run's result. A cause such as "the Mac was asleep" may be added. |
| `Say "run <name>" if you still want it.` | Ends the list of runs missed by more than 12 hours. |

<!-- messages:end -->

A scheduled run reports a failure or a skip at most once a day for each protocol and kind. A run you asked for always reports. Every run, failure, skip and change is in the audit log (`mirrin audit`): `protocol.run`, `protocol.failed`, `protocol.skipped`, `protocol.missed`, `protocol.created`, `protocol.changed` and `protocol.skip`.

### The log

The details your twin leaves out of its messages go to its log, `~/.mirrin/logs/mirrin.log` (`logs/mirrin.log` under `MIRRIN_HOME`, when that is set): why a run failed, a protocol it couldn't schedule, what a protocol needs that isn't connected, and each file it skipped. Keys are taken out before anything is written. When you ask for help, `mirrin report` writes a problem report to share, with the end of the log in it.

## Approvals

A protocol run follows the same approval rules as a chat. With the default `autonomy` settings:

- **Looking things up runs without asking:** reading the calendar and email, fetching web pages, reading files that aren't sensitive, recalling and remembering, setting reminders.
- **Writes ask first:** sending email or texts, creating or moving events, browser clicks, writing files, and running, creating or changing protocols.
- **Dangerous calls ask first:** the shell, deleting events, phone calls.
- **Some calls never run without a yes,** whatever `always_allow` or `autonomy` says: the shell, payments, a browser click that looks like a payment, and files that hold keys.
- **Tools from MCP servers** take the risk their server is given in config (write by default).

When a run needs a yes, the result it sends carries the request, and you answer `yes 12` as usual. Requests lapse after `autonomy.approval_ttl` (72 hours by default).

`autonomy.always_ask` lists tools that always ask. `autonomy.always_allow` lists tools that never ask, unless they are dangerous.

**Starting a run.** A scheduled run, Run now and the menu bar start without asking. Your twin's protocol tools are writes, so they ask by default:

- `run_protocol` asks with `run_protocol "<name>" now: <description>`: the name and description, not the prompt. Add `run_protocol` to `autonomy.always_allow` to run protocols without asking, or answer "yes, always" in your own chat.
- `create_protocol`, `update_protocol` and `install_pack` can't be given "yes, always" in chat. Only listing them in `autonomy.always_allow`, or setting `autonomy.write: auto`, would skip the question, and that isn't advised.

**Packs get your settings.** A protocol from a pack runs with exactly the same settings as your own, `always_allow` included. Per-pack permissions (which sites or files a pack may touch) are planned, not built. Read [security.md](security.md) before you install packs you don't know.

## Reloading

Nothing watches your protocols folder. After you edit a file by hand, say `/reload` in any chat with your twin (or restart it). It answers `Reloaded 7 protocols.`, or names what couldn't be scheduled.

These reload on their own:

- `mirrin protocols new`, `add`, `remove` and `install`, and `update` once it has applied an update, tell a twin running on this computer to reload;
- your twin's `create_protocol` and `update_protocol`;
- every action on the Routines page.

`/reload` also re-reads your persona file. `/protocols` in chat lists each protocol with its schedule.

## Checking

Three checks look at protocols at different moments. Each says what is wrong in plain words.

### The loader

When protocols load, a file that can't be used is skipped and the rest load. `mirrin protocols check` lists skipped files, and the Health page shows them under "Protocol and persona files" until they are fixed. A running twin tells you about each new one once: `I skipped a protocol file I couldn't use: <file> (<reason>). Fix it, then say /reload.`

<!-- messages:start -->

| Message | Why |
|---|---|
| `not valid YAML: <error>` | The file isn't valid YAML. |
| `prompt is required` | There is no prompt, or it is blank. |
| `symbolic links are not loaded from packs` | A pack's file, or its `protocols/` folder, is a symbolic link. |
| `<file> already defines "<name>"` | Two of your files, or two packs, use the same name. |
| `not valid YAML, so no vars were applied: <error>` | `vars.yaml` is broken. For a mistake in the YAML itself no protocol gets its values; a block of the wrong shape is left out and the rest apply (see [Vars](#vars)). |
| `I skipped a protocol file I couldn't use: <file> (<reason>). Fix it, then say /reload.` | Your twin's message about a new skipped file. |

<!-- messages:end -->

### Saving

When your twin creates a protocol, or saves a change to one, the protocol is checked first, so one that could never run is never written:

<!-- messages:start -->

| Message | Why |
|---|---|
| `a protocol needs a name and a prompt` | One of them is missing or blank. |
| `give the protocol a name with at least one letter or number` | The name would make an empty file name. |
| `the schedule "<schedule>" can't be run; use a cron expression with 5 fields (minute hour day month weekday), like "0 7 * * *" for 7am daily` | The schedule can't be read. |
| `; it has 6, so drop the seconds` | Added when the schedule has six fields. |
| `; it has <n>` | Added for another wrong number of fields. |
| `<file> already exists; edit it, or pick another name` | A file with that name is already in your folder. |

<!-- messages:end -->

### The scheduler

When protocols load, and when a schedule is changed from chat or the Routines page, each schedule is checked again:

<!-- messages:start -->

| Message | Why |
|---|---|
| `bad schedule "<schedule>": use five fields, minute hour day month weekday, e.g. "0 7 * * 1-5"` | The schedule can't be read. `; it has 6, so drop the seconds` is added for six fields. |
| `protocol "<name>": <why>` | How the `/reload` reply names each protocol it couldn't schedule. |
| `Reloaded <n> protocols. Not scheduled: <why>` | The `/reload` reply when a schedule couldn't be read. |

<!-- messages:end -->

## Lint rules

`mirrin protocols lint` checks a protocol, a folder or a pack for what breaks sharing. Errors make it exit with status 1; warnings and info don't. Findings are listed errors first, then info, then warnings.

<!-- lint-rules:start -->

| Level | Message | When |
|---|---|---|
| error | `not valid YAML: <error>` | The file isn't valid YAML. |
| error | `name is required` | No `name`. The loader would fall back to the file name, but a shared protocol should say its name. |
| warn | `name should be lowercase; users say it out loud` | The name has capital letters. |
| error | `prompt is required` | No prompt, or a blank one. |
| warn | `description is empty; it is what people see when they search` | No description. |
| warn | `version is empty (use semver, e.g. 0.1.0)` | No version (protocols and personas). |
| warn | `author is empty` | No author (protocols and personas). |
| error | `schedule is not a valid 5-field cron expression: <why>` | The schedule can't be read. The reason comes from the cron library, such as `expected exactly 5 fields, found 6`. |
| warn | `scheduled protocols should mention NOTHING_TO_REPORT so quiet runs stay quiet` | A valid schedule, and the prompt never says `NOTHING_TO_REPORT`. |
| warn | `requires "<name>" is not a known tool or skill` | Not a skill name, and not a tool the twin running lint has. Checked only when lint could list that twin's tools. |
| error | `prompt uses {{<var>}} but vars does not declare it` | A `{{var}}` in the prompt isn't under `vars`. |
| warn | `var "<var>" is declared but never used in the prompt` | A var the prompt never uses. |
| warn | `var "<var>" is required but has a default; drop one` | `required: true` and a `default` together. |
| error | `var "<var>" defaults to what looks like <kind>; never put credentials in a protocol` | A default looks like a secret. |
| warn | `prompt is very long; protocols work best as a brief, not an essay` | The prompt is over 4,000 bytes. |
| error | `prompt contains what looks like <kind>; never put credentials in a protocol` | The prompt looks like it holds a secret. |
| error | `pack.yaml needs name, description and repo` | A pack's `pack.yaml` is missing one of them, or isn't valid YAML. Checked only when the pack has a `pack.yaml` and at least one file was linted. |
| error | `symbolic links are not loaded from packs` | A pack's `protocols/` or `personas/` folder is a symbolic link. |
| error | `name and character are required` | A persona in the pack lacks one. |
| warn | `tagline is empty; it is what people see in the Persona menu` | A persona has no tagline. |
| warn | `id "<id>" is a built-in persona's; yours will show up as <pack>/<id> next to it, so a distinct id reads better` | A pack persona reuses a built-in persona's id. |
| info | `persona <Name> shows up as <pack>/<id> in the Persona menu` | A clean persona, so it isn't silent. |

<!-- lint-rules:end -->

**What looks like a secret.** Lint looks for the shapes of real secrets, not for words, so "Secret Santa" or "remind me to change my password" is fine. It scans the prompt and each var's `default`, not the description or `vars.yaml`. The kinds it names:

- "an API key (sk-…)", "a GitHub token", "a Slack token", "an AWS access key", "a Google API key", "a private key";
- "a password or key written out": `password`, `passwd`, `pwd`, `api key` (or `api_key`, `api-key`, `apikey`), `secret` or `token`, then `:` or `=`, then a value of six or more characters that isn't all letters and isn't a `{{var}}`.

**What lint looks at.**

- A file: that protocol only.
- A folder: every protocol directly in it, or in its `protocols/` folder when it has one, plus its `personas/` folder and `pack.yaml`. With no path, your protocols folder (installed packs aren't included).
- To check `requires`, lint loads your twin from your config, as `check` does, and asks it for its tools. If it can't be loaded, the `requires` check is skipped and the other checks still run.

## The `mirrin protocols` command

Every subcommand reads your config to find your protocols folder. Set `MIRRIN_HOME` to use another home, as the pack template's lint workflow does.

| Command | What it does |
|---|---|
| `mirrin protocols` or `mirrin protocols list` | One line per protocol: name, cron or `on demand`, `local` or `pack:<folder>`, description, and `(disabled)` when it is off. Then the installed packs. |
| `mirrin protocols new <name>` | Writes a starting file named after it (the name can be several words, quoted or not), prints its path, and tells a running twin to reload. |
| `mirrin protocols lint [path]` | Lints a file, a folder or a pack (see [Lint rules](#lint-rules)). Exits 1 on any error. A path that isn't there is an error too: `there's no file or folder at <path>`. |
| `mirrin protocols check` | Says, for this machine, which files were skipped, which protocols need something that isn't connected, and which still need vars. Always exits 0, and doesn't look at schedules. |
| `mirrin protocols add <git-url\|path>` | Installs a pack from a git address (pin it with `#tag`, `#branch` or `#<full commit>`) or copies one from a folder. |
| `mirrin protocols remove <pack>` | Removes an installed pack, by folder name or by the name in its `pack.yaml`. |
| `mirrin protocols update [--yes]` | Lists what each pack's update would change, then asks before applying. `--yes` (or `-y`) applies without asking. With nobody at the terminal to ask, nothing is applied. |
| `mirrin protocols search [term]` | Searches the pack registry by name, description and tags. No term lists every pack. |
| `mirrin protocols install <pack>` | Installs a pack from the registry by its name. |

Packs and the registry are covered in [packs.md](packs.md) and [registry.md](registry.md).

What `lint` and `check` print, from a real run (only the home folder is shortened):

```
$ mirrin protocols lint ~/.mirrin/protocols/rain-check.yaml
  rain-check.yaml: error: prompt uses {{forecast_site}} but vars does not declare it
  rain-check.yaml: warn: scheduled protocols should mention NOTHING_TO_REPORT so quiet runs stay quiet
  rain-check.yaml: warn: requires "weather" is not a known tool or skill
1 file(s), 3 finding(s)

$ mirrin protocols check
 ✗ skipped /Users/you/.mirrin/protocols/half-done.yaml: prompt is required
 ✗ chase refund             needs: email
 ✓ evening wrap             ok
 ✗ inbox triage             needs: email
 ✓ morning briefing         ok
 ! rain check               set vars in /Users/you/.mirrin/protocols/vars.yaml: city
 ✓ rebook                   ok
 ✗ reply like me            needs: email
```

`check` shows `needs:` before `set vars`, so a protocol that lacks both shows only what it needs. When nothing is wrong it ends with `all protocols ready`. With no protocols, `list` says `no protocols. Try: mirrin protocols new <name>, or mirrin protocols search`.

## In chat

Your twin has these tools for protocols. "Asks first" is with the default settings.

| Tool | Asks first? | What it does |
|---|---|---|
| `list_protocols` | no | Lists each protocol as `- <name> [<cron or on demand>]: <description>`, with `(disabled)` when it is off. |
| `run_protocol` | yes: `run_protocol "<name>" now: <description>` | Runs one now, in its own conversation, and hands the result back to the chat. |
| `create_protocol` | yes, showing the name, description, when it runs and every word of the instructions | Writes a new protocol to your folder. |
| `update_protocol` | yes, in words, such as `Move “morning briefing” from every day at 7:00 to weekdays at 8:30.` | Changes the schedule, turns it off or on, or skips its next run. |
| `find_protocols` | no | Searches the pack registry. |
| `install_pack` with `preview` | no for a registry name or an `https://` address; yes over ssh (`git@…`, `ssh://`), since that signs in with your keys | Fetches the pack into a temporary folder and shows each protocol's schedule, needs and full prompt. Installs nothing. A folder on this computer can't be previewed. |
| `install_pack` | yes | Installs a pack by registry name, git address or folder, then lists what arrived and what each protocol requires. |

**`create_protocol`** checks the protocol before you are asked, so one that couldn't be saved is never put to you. The request looks like this:

```
create_protocol "rain check": Warns on weekday mornings when rain is likely during something outdoors.
Runs on the schedule 30 6 * * 1-5 (cron, your timezone).
Instructions it will follow:
Look at today's calendar with list_events ...
```

It saves the name in lowercase, with `version: 0.1.0` and `author: Mirrin (from a conversation)`, and adds the `NOTHING_TO_REPORT` line to a scheduled prompt that lacks it. It writes no `vars` or `requires`; add those by hand.

**`update_protocol`** says the change in words before you agree. Changing a pack protocol's schedule or switch adds `This makes your own copy, so it stops getting updates from the “<pack>” pack.` It can't turn a scheduled protocol back into one that runs only when asked: an empty schedule is ignored, as if left out. Turn it off instead, or remove `schedule:` from the file and `/reload`.

<!-- messages:start -->

| Message | Why |
|---|---|
| `no protocol named "<name>"` | `run_protocol` was given a name nobody has. |
| `you are already running a protocol; do the work directly with the other tools, or say what is missing` | A protocol run tried to start another protocol. |
| `two background runs are already in progress; try again in a minute` | At most two runs started from chat go at once. |
| `<Name> can't run yet: it still needs <vars>. Add it under "<name>" in <vars.yaml>, say /reload, then try again` | A var has no value. |
| `a protocol named "<name>" already exists; to move it, turn it off or skip a run, use update_protocol` | `create_protocol` was given a name in use. |
| `no protocol named "<name>"; list_protocols shows them` | `update_protocol` was given a name nobody has. |
| `“<name>” has no scheduled run to skip: <why>` | Skip next on one that runs only when asked, or is off. |
| `say what to change: a schedule, enabled, or skip_next` | `update_protocol` was given nothing to change. |
| `no packs match; offer to create a protocol with create_protocol instead` | `find_protocols` found nothing. |
| `only packs from the registry or a git address (https:// or git@) can be previewed` | A preview of a folder on this computer. |

<!-- messages:end -->

## The Routines page

Open it from the menu bar (**Protocols…**) or from **Routines** in the links at the top of any settings page. With `mirrin run` there is no menu bar: run `mirrin devices page`, which opens the Devices page in your browser, and choose **Routines** at the top. Its heading says Protocols. It is a settings page, so it opens on this computer, or on a device paired with `admin`. Its actions are settings, not chat approvals: they happen when you press the button.

**Find more**

- Search the registry. Each result has **Preview**, which shows the protocols the pack would add: when each runs (as cron), what it needs, and the instructions it follows. After a preview the button becomes **Install**.
- Paste a pack's git URL and press **Add**. This installs at once, with no preview.

**Installed**, one row per protocol:

- name, an "off" tag when it is off, and the description;
- when it runs, in words; `local` or `pack: <folder>`; author, version and tags; and "needs: …" for anything missing;
- **Run now**: starts it and says "Started. If there's anything to report, it comes to your chat.";
- **Skip next**, shown when it is scheduled and on;
- an **On/Off** switch;
- a time and days field (Every day, Weekdays, Weekends, or Same days as now when its days are something else) with **Set time**.

Changing a pack protocol's time or switch here makes your own copy, as in [Names](#names), and the page says so.

**Packs**: each installed pack with its repository and the commit it is at, **Check for updates**, **Update** when one is ready, and **Remove** (it asks first). See [packs.md](packs.md).
