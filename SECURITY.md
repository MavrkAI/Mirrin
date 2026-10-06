# Security

Mirrin runs on your machine with your email, your browser sessions and, if you allow it, your shell. We take reports seriously and fix fast.

## Reporting a vulnerability

Use GitHub's private reporting: **Security → Report a vulnerability** on this repository. That reaches the maintainers only. Please include steps to reproduce and the version (`mirrin version`).

We aim to acknowledge within 48 hours and to ship a fix for confirmed issues within 14 days, sooner for anything that lets another party act as the user. We'll credit you in the release notes unless you'd rather we didn't.

Please don't open a public issue for anything exploitable, and please don't test against other people's twins.

## What is in scope

- The daemon, its local API, the menu bar app and the CLI.
- The channels (WhatsApp, Telegram, Discord, Slack, Signal, Matrix, Mattermost, Zulip, IRC, email, iMessage), especially anything that lets a non-owner trigger an action.
- Tool execution: browser, shell, files, custom tools, MCP.
- Prompt injection paths: web pages, emails, messages and files the twin reads.
- Protocol packs and the pack registry: installing, previewing, updating and removing packs, and the registry index and its signature.

## Harmful protocol packs

A pack in the registry that could hurt people (one that sends their data somewhere, tries to get round approvals, or acts without asking) is a security report: send it the same private way as above, with the pack's name, its repository and the commit, and what it does. Please don't open a public issue for it. For a pack that is only broken or misleading, use the **Report a pack** issue form.

What a pack can and can't do on someone's machine, what reviewers look for, and what the maintainers do when a pack has to come down are in [`docs/protocols/security.md`](docs/protocols/security.md).

## Design summary

The full threat model is in [`docs/threat-model.md`](docs/threat-model.md). In short:

- **Owner only, with proof.** Every channel filters to the configured owner. Where a name can be claimed by anyone, it has to be proven: email needs an SPF, DKIM or DMARC pass written by the receiving mail server itself, IRC needs the owner's services account or full hostmask. Mail or IRC messages that claim the owner's conversation without proof are dropped.
- **Strangers get nothing private.** Others are ignored unless `reply_to_others` is on. Then the model is told the sender is not the principal, none of the owner's memory is in the prompt, and every tool call, even a read, waits for the owner's approval, which the owner is asked for in their own chat.
- **Approvals.** Actions are classified read / write / dangerous. Writes and dangerous actions wait for the owner's explicit yes. Payment clicks always ask, with a screenshot, whatever the settings. Keys, cloud credentials, browser profiles, `.env` files and Mirrin's own files always ask before the file tools touch them. Creating or running a protocol asks, and so does every run of a tool the twin wrote for itself. Every approval shows what it is for: the whole command, script, message or prompt, not a clipped first line.
- **Egress.** `fetch_url` reaches only the public internet, checked on every connection and redirect, and before anything goes through a proxy; local and private addresses need `skills.web.allow_hosts`.
- **Least privilege for scripts.** Custom tools and MCP servers get a minimal environment plus the variables named for them, never the daemon's keys and tokens.
- **Local API** binds to loopback with a random master key; page cookies hold their own device keys and are `HttpOnly`, `SameSite`. Pairing v2 gives each device a revocable key with explicit scopes. The optional plain-HTTP `api.remote` listener also accepts legacy shared keys with limited access while migrating clients to device keys; use it only over Tailscale or a trusted network. See [Local surface](docs/threat-model.md#local-surface) for the listener rules. Provider webhooks (Twilio) verify signatures. The Google sign-in callback uses a single-use, expiring state and PKCE.
- **Secrets** live in `~/.mirrin/config.yaml` and `~/.mirrin/secrets.env` (both mode 0600; service definitions carry none) and are never sent anywhere but the service they belong to. `mirrin identity export` leaves them out.
- **Releases.** Releases after 0.2.0 carry a `SHA256SUMS` file signed with Sigstore by the release workflow. Both installers (`install.sh`, `install.ps1`) and `mirrin update` check every download against it before installing anything, and `install.sh` also verifies the signature when [cosign](https://github.com/sigstore/cosign) is installed.
- **Servers.** The free twin needs no Mirrin server. Every server a twin may talk to for reach, push, certificates or backup, and what each can see, is listed in [Servers and what each sees](docs/threat-model.md#servers-and-what-each-sees). What the optional paid service (planned) could and couldn't see, with a way to check each claim, is [`docs/cloud-trust.md`](docs/cloud-trust.md).
- **Content is data.** Everything read from the outside world is treated as untrusted input to the model, never as instructions, and tool budgets bound what one request can do. The threat model lists exactly what can still run without asking.
