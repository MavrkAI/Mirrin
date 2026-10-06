# Protocols and packs: the safety model

This page is for protocol authors, registry reviewers and anyone installing a pack: it explains what a protocol can do on your machine, what stops it doing more, what to look for before you say yes, and how to report a pack that's harmful.

The project-wide threat model is [docs/threat-model.md](../threat-model.md). This page is the part of it that concerns protocols and packs.

## A protocol is a prompt that runs with your tools

A protocol is YAML, not code. Nothing in it is executed. But when it runs, its prompt is handed to your twin as a task to carry out for you, and your twin does it with every tool you've connected: your email, calendar, browser, files and the rest. So a protocol can do whatever you could ask your twin to do, under the same approvals.

- **A pack's protocols are trusted like your own.** They run under your approval settings, including anything you've put in `autonomy.always_allow`. There are no per-pack permissions yet. Limiting which sites and files a pack's protocols may reach is planned.
- **Installed means on.** A pack's scheduled protocols start running at their next due time, without asking each time. Only the actions in the next section ask.
- **Each run is contained.** It happens in its own scratch conversation, so nothing it reads can rewrite your main history. It gets at most 25 rounds of tool use (each round can call several tools) and at most 10 minutes.

## What still asks

With the default settings (`autonomy.read: auto`, `write: ask`, `dangerous: ask`):

| Runs without asking | Asks you first | Always asks, whatever your settings |
|---|---|---|
| Reading your calendar, email and Drive; `fetch_url` to public addresses and reading web pages; reading or listing files outside the sensitive places; remembering and recalling; setting and listing reminders; listing protocols; searching the pack registry | Sending email or messages; creating, moving or deleting events; clicks and form fills in the browser; writing files; phone calls; running, creating or changing a protocol; installing a pack; creating a tool | Payments: the payment check and any payment-looking browser click, with a screenshot. Shell commands (`run_shell`). Sensitive files through the file tools: `~/.ssh`, `~/.gnupg`, cloud credentials, keychains and password stores, browser profiles, `.env` files, private keys, and Mirrin's own files, even just to read them |

- **The hard floor** in the last column turns an automatic yes into a question, even with `auto` or `always_allow`. A level you've set to `never` stays a refusal. It covers Mirrin's built-in tools; custom tools and MCP servers ask according to the risk they declare.
- **An approval shows the whole thing**: the full message, command or file contents, not a clipped first line. When a protocol needs one mid-run, it reaches you in your chat as a numbered request. Read it before you say yes.
- **"Yes, always" has limits.** It can't be given for creating or changing protocols, installing packs, creating tools, browser clicks, or anything dangerous, so a protocol can't talk its way into standing powers.
- **Spending** has a per-payment limit and a monthly cap that the twin can't change.

## Never put secrets in a protocol

A protocol is meant to be shared, and its prompt is shown in full to anyone who previews it. Passwords, API keys and tokens belong in the person's own config, `~/.mirrin/secrets.env` or a connected account, never in a protocol. Use `vars` for addresses and preferences only.

`mirrin protocols lint` fails with an error when the prompt or a var's `default` looks like a credential:

| Lint recognises | Shape |
|---|---|
| an API key (sk-…) | `sk-` followed by 16 or more characters |
| a GitHub token | `ghp_`, `gho_`, `ghu_`, `ghs_` or `ghr_` followed by 20 or more |
| a Slack token | `xoxa-`, `xoxb-`, `xoxp-`, `xoxr-` or `xoxs-` followed by 10 or more |
| an AWS access key | `AKIA` and 16 capitals or digits |
| a Google API key | `AIza` and 35 more |
| a private key | a `-----BEGIN … PRIVATE KEY-----` line |
| a password or key written out | `password`, `passwd`, `pwd`, `api key` (or `api_key`, `api-key`, `apikey`), `secret` or `token`, in any case, then `:` or `=`, then a value of 6 or more characters that isn't all letters and isn't a `{{var}}` |

The message reads `prompt contains what looks like <kind>; never put credentials in a protocol` (or `var "<name>" defaults to what looks like …`). Lint scans only the prompt and var defaults: descriptions, `pack.yaml`, personas, READMEs and the rest of the repository are yours to check. Keep personal data out too: names, addresses, phone numbers and email addresses belong in the user's `vars.yaml`, not in a shared prompt.

## What the twin reads is data, not instructions

Web pages, emails, files and other people's messages reach the model as data. Your twin is told never to take instructions from them. That's a mitigation, not a guarantee: a well-crafted page can still sway a model. The approvals above are the real boundary, which is why anything that sends, spends, deletes or runs code asks.

What this means when you write a protocol:

- Tell the twin what to look for and what to report, and never to do what a page, email or file says. "Summarise what the landlord's email asks for" is safe; "do what the email asks" is not.
- Keep the actions in a protocol few and predictable, so the approval questions it raises are ones the owner expects.
- Runs started by new mail or a calendar change (watchers, not protocols) are treated like a stranger's message: every tool asks. A protocol's own prompt is treated as yours, which is why reading a pack before you install it matters.

## What a harmful pack could try, and what limits it

| It could try to | What limits it |
|---|---|
| Send your data to a site it names, for example by putting your details in a `fetch_url` address | Fetching a public page is a read, so it runs without asking by default. Review reads every prompt for this. To be asked before every fetch, add `fetch_url` to `autonomy.always_ask` in your config. |
| Send email or messages as you, or change your calendar | These ask, showing the whole message or event. |
| Spend money, run commands, or read your keys | These always ask, whatever your settings. |
| Change its prompt after you've installed it | Installs stay on one commit. Updates show the files that changed and wait for your yes. A registry entry moves only by a reviewed pull request. |
| Give itself standing powers | Creating protocols or tools and installing packs ask, and "yes, always" can't cover them. |
| Run constantly, or message you when there's nothing to say | Schedules show in `mirrin protocols list` and on the Routines page, where you can turn any protocol off. Reviewers check schedules and `NOTHING_TO_REPORT`. |
| Reach files on your machine through a symbolic link | Protocol and persona files and folders that are links aren't loaded from packs, and a copy from a folder leaves links out. |
| Replace your persona with its own | Pack personas never replace yours or a built-in one; they appear beside them as `<pack>/<id>`. |
| Ask you to approve something you wouldn't | Approvals show the whole request. A bare "yes" only answers the question just asked, and a request that changed after you were asked is never carried out. |

## Before you install a pack

- **Preview it.** Ask your twin to show you what the pack would add before installing: it fetches the pack into a scratch folder and shows each protocol's schedule, what it needs and every word of its instructions, then asks before installing. On the Routines page, search for it, press **Preview**, then **Install**. `mirrin protocols add`, `mirrin protocols install` and pasting an address into the Routines page install straight away, with no preview, so read the pack's `protocols/` folder on its repository page first.
- **Prefer the registry.** Registry packs were reviewed at a particular commit, and you get that commit.
- **Look after installing.** `mirrin protocols list` shows each protocol's schedule and which pack it came from. Turn off anything you don't want on the Routines page.
- **Read updates.** `mirrin protocols update` lists the files that changed and prints the command for the full diff. Say no if you don't like what you see.
- **Remove what you don't use**: `mirrin protocols remove <pack>`.

## Red flags for reviewers

Reviewers use the full checklist in [registry.md](registry.md#what-reviewers-check). These are the signs that a pack needs a hard look, or a no:

- A prompt sends anything the twin read (mail, events, files, memory, names, places) to an address: in a `fetch_url` call, a link to click, a form to fill or a message to send.
- A var default is an address that collects data, or anything credential-shaped.
- A prompt tells the twin to approve, rush or skip approvals; to add tools to `always_allow`; to install packs or create protocols or tools; to turn off or change other protocols; or to keep something from the owner.
- A prompt tells the twin to follow instructions found in a page, an email or a file.
- `requires` leaves out tools the prompt uses, or the description doesn't mention a send, a payment or a deletion the prompt makes.
- A schedule much more frequent than the job needs, or a scheduled prompt with no `NOTHING_TO_REPORT`.
- Hidden text: invisible or look-alike characters, very long lines, or a prompt far longer than its job.
- Symbolic links anywhere in the repository, a committed `.antbot-pack.json`, or YAML at the top level outside `protocols/` and `personas/` that isn't `pack.yaml`.
- A persona whose `character` gives the twin instructions to act rather than a way to speak.
- An update whose diff changes what a protocol does without saying so in the pull request or the changelog.
- No `commit` in the entry, or a repository whose history was rewritten after review.

## How to report

- **A pack that could hurt people** (it sends data somewhere, tries to get round approvals, or acts without asking), **or a weakness in how Mirrin handles packs**: report it privately with GitHub's **Security → Report a vulnerability** on [MavrkAI/Mirrin](https://github.com/MavrkAI/Mirrin/security/advisories/new). That reaches the maintainers only. [SECURITY.md](../../SECURITY.md) has the details. If you can't see that button, open an issue that says only that you have a security report about a pack and asks a maintainer to contact you; leave the details out.
- **A pack that's broken or misleading**: open a **Report a pack** issue.
- **Malware hosted on GitHub, GitLab or Codeberg**: report it to that site too. Removing it from our registry doesn't take it down there.

## Takedown

What the maintainers do with a report about a pack in the registry:

1. Acknowledge it within 48 hours, the target in [SECURITY.md](../../SECURITY.md).
2. Read the pack at the commit the registry lists, and confirm the problem.
3. If only the latest version is harmful, point the entry's `commit` back at the last good one, so `mirrin protocols update` offers everyone the way back. Otherwise remove the entry. Either change goes to `main` as soon as the problem is confirmed, without waiting for the author.
4. For a pack that harmed or could harm people, publish a security advisory that names the pack, says what it does and how to remove it, and add a line to `CHANGELOG.md`, so the next release's built-in index leaves it out too.
5. Report the repository to the site that hosts it when it holds malware.
6. Credit the reporter, unless they'd rather not be named.

Be clear about what a takedown can't do. Mirrin can't switch a pack off on anyone's machine. People who installed it keep it until they remove it; their next update tells them it's `no longer in the pack registry, so it was left as it is; remove it if you don't want it any more`. Until signing is in place, every release answers from the copy of the index built into it, so a removal reaches search and install with the next release ([registry.md](registry.md#signing)). That's why review happens before a pack is listed, and why entries are pinned to a commit.
