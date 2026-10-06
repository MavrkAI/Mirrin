# Writing a good protocol

This page is for anyone writing a protocol, for themselves or to share: it shows how to write one that does its job, stays quiet when it has nothing to say, and that people keep switched on.

The format itself is in the [reference](reference.md). If you haven't written one yet, the [quickstart](quickstart.md) takes ten minutes.

## One job per protocol

"Morning briefing" is a protocol. "Everything about my day" is not. A protocol with one job is easy to schedule, skip and switch off, and it knows when it has nothing to say. If you find yourself writing "and also", you have two protocols.

## Brief it like a colleague

The prompt is written to your twin, in the second person, the way you'd brief a capable colleague who has never done this job before. Say four things:

- **What to check.** Name the tools and the sources: "today's calendar with list_events", "the forecast for {{city}} from {{forecast_site}} with fetch_url".
- **What matters.** The threshold that turns a look into news: "rain is likely during something outdoors", "an email that needs a reply or a decision".
- **What to say.** The shape and the length: "in one or two sentences: which event, what time, and what to bring".
- **When to say nothing.** "If nothing is outdoors, or no rain is expected, reply NOTHING_TO_REPORT."

Then let your twin decide how. Don't script every tool call; say what a good result looks like.

A few things to know as you write:

- **Each run starts fresh.** A run has a conversation of its own and doesn't carry on from the last one. Put everything it needs in the prompt.
- **Keep it a brief.** Lint warns when a prompt passes 4,000 bytes. A long prompt is usually two protocols.
- **Acting needs a yes.** If the protocol should send, book or change something, say so plainly. With the default settings your twin asks before any of that, whatever the prompt says. A run doesn't wait for the yes: it ends, and the message it sends carries the request, which can be answered for up to three days. So brief it to draft and ask ("draft a reply and ask before sending it"), not to report something as done.

## Quiet when nothing matters

A protocol that talks when there's nothing to say gets switched off. On a scheduled run or Run now, if your twin's reply contains `NOTHING_TO_REPORT` anywhere, nothing is sent. So:

- Say exactly when to reply `NOTHING_TO_REPORT`, as a condition it can test: "If nothing touches on {{interests}}", not "if it's boring".
- Use the token as written, in capitals with underscores. "Nothing to report" in plain words is sent like any other reply.
- Don't ask for a sign-off or a summary line on quiet days. Silence is the summary.
- A run that arrives late (the laptop was asleep) is told how late, and can stay quiet if the moment has passed. Help it: "If it is after 9:00, the commute has happened; reply NOTHING_TO_REPORT."

A protocol that runs only when asked can still use the token for Run now. Asked in chat, the result goes back into the conversation, so your twin always answers there.

## Vars that are safe

Vars hold what differs between people: a city, a suburb, the topics they follow, a page to read.

- **Never a secret.** No passwords, API keys or tokens, in a var or anywhere in the prompt. Accounts are set up in your twin's own settings, and its tools use them. `mirrin protocols lint` flags anything in the prompt or a default that looks like a real key, token or written-out password.
- **Default what you can, require what you can't.** A public forecast page makes a good default, so nobody has to fill it in. A home city has no sensible default, so leave it empty and mark it `required`. Don't do both: lint warns.
- **Nothing of yours.** When you share, your own address, email or name must not be left in a default.
- **Describe it as a noun phrase.** The description is quoted when a value is missing: "it still needs city (the city to check the forecast for)".
- **Name it plainly.** Var names take ASCII letters, digits and `_` only: `{{home_city}}`. Anything else in braces is left in the prompt as written.
- **Write around the value.** It is pasted into the prompt as text, so "the forecast for {{city}}" reads well whatever city it is.

## Requires, honestly

`requires` is how people know, before they install, what a protocol needs. It is never enforced: a protocol that needs a calendar runs without one, and does worse. So be honest.

- List everything the prompt can't do without, and nothing it doesn't use.
- Prefer skill names (`calendar`, `email`, `web`). They pass lint on any machine, and a skill is met by any of its tools: `email` is IMAP or Gmail.
- In the prompt, name the alternatives: "list_emails, or gmail_search with Gmail". The starters do this.
- Make optional parts optional in the prompt, rather than in `requires`: "If email is available, note anything that needs a reply."

## Schedules people will like

- **As rare as the job allows.** Every run uses the model and counts towards the monthly model budget; once that is used up, background runs stay quiet until the budget is raised or the month turns. `*/30 * * * *` is 48 runs a day.
- **Just before the decision it informs.** Rain check runs at 6:30, before people leave, not at 9:00.
- **Not at night.** Quiet hours don't hold a routine's result. A protocol scheduled for 23:30 lights up a phone at 23:30.
- **Weekdays when it's about work:** `1-5` in the last field.
- **No time zone.** Schedules run in each person's own zone. Write five plain fields.
- **On demand when timing is personal.** Leave `schedule` out, and people run it when they need it.
- **`briefing` only for a summary of the day.** That tag hands it what was held overnight; see the [reference](reference.md#briefings).

## Write for the ear

A result lands in a chat app or on a lock screen, and it may be read aloud: to someone in the room who has been talking to their twin, or when they asked for it by voice. Write it so it sounds right spoken.

- Lead with the point, in one to three short sentences.
- No tables, headings, walls of bullets or raw links.
- Times and dates the way people say them: "at half past six", "on Tuesday the 6th", not `2026-10-06T06:30`.
- Amounts with their currency, rounded the way people say them: "about $40", "$12.50".
- Tell your twin the shape in the prompt: "write it the way you'd say it out loud".

## Names in any language

A name is what people say and type, so pick one that is easy to say and to spell.

- Any script works. The file name keeps letters and digits in any script, so `朝のまとめ` is saved as `朝のまとめ.yaml`.
- Keep it lowercase where the script has case. Lint warns otherwise, and your twin matches names ignoring case anyway.
- The key in `vars.yaml` must match the name exactly, including case and script.
- Var names are ASCII only, whatever language the prompt is in.
- Write the prompt in the language that suits you. If the answer should be in a particular language, say so.
- A name people already have hides yours: their own file wins over a pack's. Don't call yours `morning briefing`.

## Test it on real data

Lint checks the file; only real runs check the judgement.

- **Ask in chat.** "Run rain check" brings the result back into the conversation, quiet or not, so you see what it decided.
- **Make it speak.** Try it on a day it should have something to say. For rain check, put something outdoors on today's calendar, set `city` in `vars.yaml` to somewhere it is raining today, then `/reload`.
- **Make it stay quiet.** Then try it on a day it shouldn't, and check that nothing arrives from **Run now**.
- **Make it fail.** Remove a value from `vars.yaml`, say `/reload`, and run it, to read the message people get.
- **Watch it on schedule.** Set the schedule a few minutes ahead, `/reload`, and wait. `mirrin audit` shows `protocol.run`, `protocol.failed` and `protocol.skipped`.
- **Lint it as a stranger would.** A pack's lint workflow runs `mirrin protocols lint .` in a throwaway home, with nothing connected. Do the same: `MIRRIN_HOME=$(mktemp -d) mirrin protocols lint .`

## Versions

`version` is shown on the Routines page, and nothing compares it: people get your updates by commit (see [packs.md](packs.md)). The version is how they read what changed, so use semver and mean it.

- **Patch** (0.1.0 to 0.1.1): better wording or judgement, with the same inputs, timing and needs.
- **Minor** (0.1.1 to 0.2.0): something new that asks nothing of anyone, such as a new var with a default, or a new protocol in the pack that runs only when asked.
- **Major** (0.2.0 to 1.0.0, and on): anything that stops a working setup or changes it under someone:
  - renaming the protocol: their `vars.yaml` block no longer matches, "run rain check" stops working, and their own copy no longer replaces yours;
  - renaming or removing a var, or adding one with no default, so their runs stop with "it still needs …";
  - a new requirement the prompt can't do without;
  - moving the schedule;
  - a new scheduled protocol in the pack, which starts running on everyone's twin once they update;
  - making a quiet protocol talk more.

People who changed your protocol's schedule or switch from chat or the Routines page now have their own copy of it, and don't get your updates to it at all. Keep names stable; a renamed protocol is a new protocol to everyone who has the old one.

## From weak to strong

A first draft:

```yaml
name: News
schedule: "*/30 * * * *"
prompt: |
  Check the news and tell me everything important that happened.
  Use a table with the headline, source and link for each story.
```

Lint passes it, with five warnings and no errors:

```
  news.yaml: warn: name should be lowercase; users say it out loud
  news.yaml: warn: description is empty; it is what people see when they search
  news.yaml: warn: version is empty (use semver, e.g. 0.1.0)
  news.yaml: warn: author is empty
  news.yaml: warn: scheduled protocols should mention NOTHING_TO_REPORT so quiet runs stay quiet
1 file(s), 5 finding(s)
```

Lint can't see the rest. It runs 48 times a day and never stays quiet. "Everything important" has no threshold and no source. A table can't be read aloud. It needs the web but doesn't say so. Rewritten:

```yaml
name: headlines
description: Twice a day, the one or two stories that touch on what you follow, or nothing.
version: 1.0.0
author: your-handle
tags: [news, daily]
schedule: "0 8,18 * * *"
requires: [web]
vars:
  interests:
    description: The topics you follow, in a few words
    required: true
  source:
    description: A news front page fetch_url can read
    default: https://www.abc.net.au/news
prompt: |
  Fetch {{source}} with fetch_url and read the stories from the last twelve hours.
  Pick at most two that touch on {{interests}}. For each, say in one or two plain
  sentences what happened and why it might matter to the user. No lists, tables or
  links: write it the way you would say it out loud.
  If nothing touches on {{interests}}, reply NOTHING_TO_REPORT.
```

What changed:

- It runs twice a day, every day at 8:00 and 18:00, instead of every half hour.
- One job with a threshold: at most two stories, and only ones that touch on what this person follows.
- A source with a sensible default, and the one thing only the reader can say, `interests`, left for them to fill in.
- An answer that reads aloud, and silence when nothing qualifies.
- An honest `requires`, a lowercase name, and a description, version and author.

It lints clean: `1 file(s), 0 finding(s)`.

## Before you publish

- [ ] `mirrin protocols lint` on the whole pack shows no errors and no warnings.
- [ ] The name is lowercase, easy to say, and not one people already have.
- [ ] The description is one line that says what it does and when.
- [ ] It does one job.
- [ ] The prompt says what to check, what matters, what to say, and exactly when to reply `NOTHING_TO_REPORT`.
- [ ] `requires` covers every tool the prompt can't do without, as skill names where possible, and nothing else.
- [ ] Every var is used, declared and described; defaults are sensible and public.
- [ ] No secrets and nothing personal of yours, in the prompt or the defaults.
- [ ] The schedule is a time people will welcome, and as rare as the job allows.
- [ ] The result reads well aloud: short sentences, no tables.
- [ ] You ran it on a day with something to say, a day with nothing, and with a value missing.
- [ ] `version` follows the rules above, and `author` is set.

Then share it: [packs.md](packs.md) shows how to put it in a pack, and [security.md](security.md) what people will weigh before they install it.
