# Your first protocol in ten minutes

This page is for you if Mirrin is set up and you want your twin to do one thing on its own: in about ten minutes you'll write a protocol, check it, run it and put it on a schedule.

You'll build **rain check**. On weekday mornings it looks at your calendar, and if rain is likely during something outdoors, it tells you what to bring. Otherwise it says nothing.

## Before you start

- Mirrin is installed and your twin is running (the menu bar app, or `mirrin run`).
- You can talk to your twin: in your messaging app, or with `mirrin chat` in a terminal.
- Rain check reads your calendar, so connect it if you can ([docs/skills.md](../skills.md)). If you don't, step 4 shows you what's missing, and you can still follow along.

## 1. Make the file

```sh
mirrin protocols new rain check
```

```
wrote /Users/you/.mirrin/protocols/rain-check.yaml
edit the prompt, then `mirrin protocols check`. To share it, put it in a pack (see docs/protocols/README.md).
```

That file is already a working protocol, one that runs only when asked:

```yaml
name: rain check
description: What it does, in one line.
version: 0.1.0
author: your name or handle
tags:
    - example
requires:
    - web
vars:
    topic:
        description: What to look for
        default: the weather
prompt: Look up {{topic}} with fetch_url and tell the user the one thing that matters, in two sentences.
```

## 2. Write what it should do

Open `~/.mirrin/protocols/rain-check.yaml` in any text editor and replace it with a first draft:

```yaml
name: rain check
description: Warns on weekday mornings when rain is likely during something outdoors.
version: 0.1.0
author: your-handle
tags: [weather, daily]
schedule: "30 6 * * 1-5"
requires: [calendar, web, weather]
vars:
  city:
    description: The city to check the forecast for
    required: true
prompt: |
  Look at today's calendar with list_events and pick out anything that happens
  outdoors or means walking somewhere. Fetch the forecast for {{city}} from
  {{forecast_site}} with fetch_url. If rain is likely during any of those, say so
  in one or two sentences: which event, what time, and what to bring.
```

What each part does:

- `name` is what you'll say to run it. Keep it lowercase.
- `schedule` is cron: minute, hour, day of month, month, day of week. `30 6 * * 1-5` is weekdays at 6:30, in your twin's time zone.
- `requires` lists what the prompt needs, so anyone can see before they run it.
- `vars` are the parts each person fills in, used as `{{city}}` in the prompt.
- `prompt` is the brief your twin follows, written to it as you'd brief a colleague.

This draft has three mistakes in it, on purpose. The next step finds them.

## 3. Lint it

```sh
mirrin protocols lint ~/.mirrin/protocols/rain-check.yaml
```

```
  rain-check.yaml: error: prompt uses {{forecast_site}} but vars does not declare it
  rain-check.yaml: warn: scheduled protocols should mention NOTHING_TO_REPORT so quiet runs stay quiet
  rain-check.yaml: warn: requires "weather" is not a known tool or skill
1 file(s), 3 finding(s)
```

Lint exits with status 1 when it finds an error. Each finding says what to fix:

- **`{{forecast_site}}`** is used but never declared. Declare it under `vars`, with a default so nobody has to fill it in.
- **`NOTHING_TO_REPORT`** is how a scheduled protocol stays quiet. If your twin's reply contains it, nothing is sent. Say when to use it.
- **`weather`** isn't a skill or a tool. The forecast comes from a web page, which the `web` skill already covers, so drop it.

The fixed file:

```yaml
name: rain check
description: Warns on weekday mornings when rain is likely during something outdoors.
version: 0.1.0
author: your-handle
tags: [weather, daily]
schedule: "30 6 * * 1-5"
requires: [calendar, web]
vars:
  city:
    description: The city to check the forecast for
    required: true
  forecast_site:
    description: A forecast page fetch_url can read
    default: https://wttr.in
prompt: |
  Look at today's calendar with list_events and pick out anything that happens
  outdoors or means walking somewhere. Fetch the forecast for {{city}} from
  {{forecast_site}} with fetch_url. If rain is likely during any of those, say so
  in one or two sentences: which event, what time, and what to bring.
  If nothing is outdoors, or no rain is expected, reply NOTHING_TO_REPORT.
```

```sh
mirrin protocols lint ~/.mirrin/protocols/rain-check.yaml
```

```
1 file(s), 0 finding(s)
```

Every rule lint checks, with its exact message, is in the [reference](reference.md#lint-rules).

## 4. Check it on this machine

Lint checks the file. `check` checks it against your own twin: what's connected, and which values you've filled in.

```sh
mirrin protocols check
```

```
 ✗ chase refund             needs: email
 ✓ evening wrap             ok
 ✗ inbox triage             needs: email
 ✓ morning briefing         ok
 ! rain check               set vars in /Users/you/.mirrin/protocols/vars.yaml: city
 ✓ rebook                   ok
 ✗ reply like me            needs: email
```

Your other protocols are listed too: these are the starters a fresh install comes with, on a twin with no email connected. A `✗` line names what isn't connected.

The sample assumes a calendar is connected. Without one you'll see `✗ rain check  needs: calendar` (and the same for rebook) instead of the `!` line, because `check` shows a missing var only once nothing else is missing. Add the city anyway. Rain check still runs without a calendar, but it can't see your day.

The `!` line says rain check still needs a city. Put yours in `~/.mirrin/protocols/vars.yaml`, under the protocol's exact name:

```yaml
rain check:
  city: Melbourne
```

Run `check` again, and rain check is ready:

```
 ✓ rain check               ok
```

Without a calendar it still says `needs: calendar`, and that's all that's missing.

## 5. Tell your twin

Nothing watches your protocols folder. `mirrin protocols new` told your twin about the file, but you've edited it since, so in any chat with your twin, say:

```
/reload
```

It answers with how many protocols it loaded, such as `Reloaded 7 protocols.` Say `/reload` after every edit by hand.

## 6. Run it now

You don't have to wait for 6:30. There are two ways to run it now.

**On the Routines page.** Open it from the menu bar (**Protocols…**), find rain check under Installed and press **Run now**. The page says "Started. If there's anything to report, it comes to your chat." If the forecast is dry, you hear nothing: that is the protocol working.

With `mirrin run` there's no menu bar. Run `mirrin devices page`, which opens the Devices page in your browser, and choose **Routines** at the top. Or use chat for this step and step 8.

**In chat.** Say "run rain check". Running a protocol is a write, so your twin asks first, and the request shows the protocol's name and description:

```
run_protocol "rain check" now: Warns on weekday mornings when rain is likely during something outdoors.
```

Reply `yes` with the number it gives you. The result comes back into the conversation, so your twin tells you what it found even when there is nothing to report. That makes chat the better way to test.

To stop being asked each time, answer "yes, always" in your own chat, or add `run_protocol` to `autonomy.always_allow` in your config.

## 7. Read the result

A scheduled run, or Run now, sends its result to your first messaging app with a chat of your own (WhatsApp, Telegram, iMessage and so on). With none, it goes to the presence screen with a desktop notification, `Rain check is ready.`

If something goes wrong, you hear that too, in plain words:

- `Rain check didn't run this time: <reason>.` A scheduled protocol adds `I'll try again next time it's due.`
- `Rain check didn't run: it still needs city (the city to check the forecast for). Add it under "rain check" in ~/.mirrin/protocols/vars.yaml, then say /reload.` Your twin reads `vars.yaml` only when protocols load, so the city counts once you've said `/reload`.

For a scheduled run you get each kind of notice at most once a day. Every run is in the audit log: `mirrin audit` lists the latest entries, such as `protocol.run`.

## 8. Schedule it

Rain check is already scheduled for weekdays at 6:30. To change when it runs, pick one:

- **The Routines page.** Set a time, choose Every day, Weekdays or Weekends, and press **Set time**. **Skip next** skips one run, and the switch turns it off.
- **Chat.** Say "move rain check to 7:15 on weekdays". Your twin puts the change in words before you agree: `Move “rain check” from weekdays at 6:30 to weekdays at 7:15.` "Skip tomorrow's rain check" and "turn off rain check" work too.
- **The file.** Edit `schedule`, then say `/reload`.

Some schedules to start from:

| Schedule | Runs |
|---|---|
| `0 7 * * *` | every day at 7:00 |
| `30 6 * * 1-5` | weekdays at 6:30 |
| `0 9 * * 6,0` | weekends at 9:00 |
| `0 17 * * 5` | Fridays at 17:00 |

If the computer is asleep at 6:30, rain check runs once when it wakes, as long as that's within 12 hours, and your twin is told it's late. The rest of the rules are in the [reference](reference.md#schedules).

## 9. Share it

Rain check works for anyone who fills in a city. To share it, put it in a pack: a git repository with a `pack.yaml` and a `protocols/` folder that others install with one command. [packs.md](packs.md) shows how, and [registry.md](registry.md) shows how to list it so people can find it.

Before you share, run `mirrin protocols lint` once more. It flags anything in a protocol that looks like a real API key, token or written-out password.

## Next

- [writing-guide.md](writing-guide.md): how to write protocols people keep switched on.
- [reference.md](reference.md): every field, rule and message.
