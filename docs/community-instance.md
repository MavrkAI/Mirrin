# Running a public twin in your Discord

The fastest way to let people try Mirrin is to put a live twin in a Discord channel. It answers everyone there, but takes no actions for them and shares nothing private.

```yaml
name: Mirrin
persona: mirrin
channels:
  discord:
    enabled: true
    token: "…"
    owner: "<your Discord user id>"
    reply_to_others: true        # answer everyone…
    channels: ["<channel id>"]   # …but only in this channel (plus @mentions elsewhere)
autonomy:
  read: auto
  write: never                   # a public instance should not send, spend or change anything
  dangerous: never
skills:
  browser: { enabled: true, headless: true }
  system: { enabled: false }     # no shell, no files
  email: { enabled: false }
spending: { monthly_limit: 0 }
```

Run it on a small VM with an Ollama model or a capped API key, and pin a message that says what it is and that it takes no actions for non-owners. People will screenshot it; that's the point.
