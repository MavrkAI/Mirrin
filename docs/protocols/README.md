# Protocols

This page is for anyone who wants their twin to do things on its own, and for anyone who builds on that: it says what a protocol is and which page to read next.

A protocol is one YAML file that your twin runs on a schedule or when you ask. It names a routine ("rain check"), says when it runs, and briefs your twin in plain words: what to check, what matters, what to say, and when to say nothing. There is no code to compile and nothing to register. Put the file in `~/.mirrin/protocols`, or install a pack someone else wrote.

## Who these pages are for

- **You want your twin to do something for you.** Start with the [quickstart](quickstart.md), then read the [writing guide](writing-guide.md).
- **You write protocols for other people.** Read the [writing guide](writing-guide.md), then [packs.md](packs.md) to share them and [registry.md](registry.md) to list them. [security.md](security.md) says what people will weigh before they install.
- **You want to improve the engine itself:** the loader, scheduler, lint or the pack tools. Read [contributing.md](contributing.md), with the [reference](reference.md) as the record of how things behave today.

## A protocol in thirty seconds

```yaml
name: rain check
description: Warns on weekday mornings when rain is likely during something outdoors.
version: 0.1.0
author: your-handle
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

Save it as `~/.mirrin/protocols/rain-check.yaml` and put your city in `~/.mirrin/protocols/vars.yaml`:

```yaml
rain check:
  city: Melbourne
```

Say `/reload` to your twin. From then on, at 6:30 on weekdays in your time zone, it looks at your day and the forecast. If you'll need an umbrella, you get a message; if not, you hear nothing. Say "run rain check" to try it now.

## The pages

| Page | What it covers |
|---|---|
| [quickstart.md](quickstart.md) | Your first protocol in ten minutes: make it, lint it, check it, run it, schedule it. |
| [writing-guide.md](writing-guide.md) | How to write one people keep switched on: one job, a clear brief, quiet when nothing matters, safe vars, honest requirements, kind schedules, answers that read aloud, testing, versions, and a checklist. |
| [reference.md](reference.md) | Every field, schedule rule, var rule, lint rule and message, the `mirrin protocols` command, the chat tools and the Routines page. |
| [packs.md](packs.md) | Putting protocols in a pack, and installing, pinning, updating and removing packs. |
| [registry.md](registry.md) | The index `mirrin protocols search` reads, and how a pack gets listed in it. |
| [security.md](security.md) | What a protocol can do on your machine, what asks first, and how to judge a pack before you install it. |
| [contributing.md](contributing.md) | Improving the protocol engine and its examples: where the code is, how to test, and how changes are reviewed. |

## Commands at a glance

```sh
mirrin protocols new rain check     # start a protocol from a template
mirrin protocols lint               # check your files for what breaks sharing
mirrin protocols check              # what's missing on this machine
mirrin protocols                    # list your protocols and packs
mirrin protocols search travel      # find packs in the registry
mirrin protocols install <pack>     # install one by name
```

In chat, `/reload` picks up files you edited and `/protocols` lists what is loaded. You can also just ask: "run rain check", "move rain check to 7:15 on weekdays", "is there a protocol for commuting?".

## Where things are

- Your protocols: `~/.mirrin/protocols/*.yaml`.
- Your values for their vars: `~/.mirrin/protocols/vars.yaml`.
- Installed packs: `~/.mirrin/protocols/packs/`.
- The starters a fresh install comes with: [`examples/protocols`](../../examples/protocols).
- A pack to copy, with a lint workflow: [`examples/pack-template`](../../examples/pack-template).
- The code: `internal/protocols` (format, lint, packs, registry), `internal/heartbeat` (schedules and runs), `internal/skills/protocols` (chat tools) and `cmd/mirrin` (the `mirrin protocols` command).
