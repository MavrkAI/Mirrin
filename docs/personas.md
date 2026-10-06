# Personas

A persona is who your twin is: its name, its character, how it speaks, what it sounds like. It is one YAML file, so it can be written, shared and installed like a protocol. The name you give your twin (`name:` in config, or "What will you call your twin" at setup) is separate from the persona: pick a personality, call it what you like.

## The file

```yaml
id: mirrin                        # identifier (file name)
name: Mirrin                      # default name
pronunciation: Mirrin             # how the voice says it, if that differs from the spelling
tagline: The butler who's two steps ahead.
author: Mirrin
version: 1.0.0
tags: [butler, dry]
address: sir                      # how it addresses the user ("" = by name)
character: |
  A prose brief, not a feature list: temperament, humour, loyalties, how it
  treats the user, what it cares about, what it never does.
style:
  - Lead with the answer, then the one or two details that matter.
  - No emoji unless the user uses them first.
greeting: "Good to see you{, address}."   # said when a voice session starts
hellos:                           # after a quiet spell, and on the screen: one for each part of the day
  morning: "Good morning{, address}."       # 5am to noon
  afternoon: "Good afternoon{, address}."   # noon to 5pm
  evening: "Good evening{, address}."       # 5pm to 11pm
  late: "Late one{, address}."              # 11pm to 5am
acks: ["Right.", "Let me see.", "On it."]   # the moment the user stops talking
voice: bm_george                  # Kokoro id, macOS or ElevenLabs voice name
voice_pitch: 0                    # Kokoro only: shift in cents (100 = a semitone); the penguin's is 520
voice_speed: 1.0                  # Kokoro only: pace, with this voice
wake_word: mirrin
wake_aliases: [mirin, mirren]     # misrecognitions to accept
wake_model: hey_mirrin.onnx       # optional openWakeWord model in ~/.mirrin/tts, used when it is there
```

Only `name` and `character` are required. In `greeting` and `hellos`, `{name}` is the owner's first name, `{address}` how the twin addresses them, and `{, address}` is ", sir" or nothing. A wall screen paired only to look says a plain "Good morning" instead, without the name. Switching persona from the menu bar brings its own voice, pitch, pace and wake word. Quote a `style` line that contains a colon followed by a space, or YAML reads it as a key. Everything about behaviour that is not character (memory, approvals, discretion, safety) is the same for every persona; the persona shapes voice and manner.

## Where they live

- Bundled: `mirrin` (Mirrin, the gentleman-butler, drawn on the screen as a man in a dinner jacket; the default; `persona: mavrk`, his id before he took the product's name, still means him), `pickoo` (a cheerful penguin with a pitched-up voice, drawn as the penguin) and `nyra` (Nyra, warm and composed, drawn as a woman in a navy blazer). Any other persona is drawn as the penguin. The bundled persona `plain` has retired: a config.yaml that still names it means Mirrin, and the next save writes `mirrin`.
- Yours: `~/.mirrin/personas/*.yaml`. A file with a bundled id overrides it.
- From packs: `packs/<pack>/personas/*.yaml`, installed with `mirrin protocols add`. Their ids are namespaced as `<folder>/<id>`, where the folder is named after the last part of the pack's address (for example `mirrin-pack-starter/butler`), so a pack can add characters but never replaces yours or a bundled one.

A file that can't be read (bad YAML, or no `name` or `character`) is skipped and the rest still load; `mirrin persona list` and the daemon log say which file and why.

`mirrin persona list | show | use | new`, or the Persona menu in the menu bar. Switching from the menu applies live. `persona new` never overwrites a file and prints the id to use.

The wake word, wake model and voice follow the persona unless you set your own in config (`channels.voice.wake_word`, `wake_model`, `voice`); yours are kept across switches and restarts. A config that names a pack persona by its old bare id (`butler`) still finds it, and is saved with its folder (`mirrin-pack-starter/butler`) the next time settings change.

## Wake words

Saying the twin's name works for any name, through transcribe-and-match: the microphone is transcribed locally in short segments and the name is matched in the text. No wake-word model ships with Mirrin, so that includes the default persona, Mirrin, until a licence-clean "Hey Mirrin" model exists (`docs/wake-word.md` says why). A name with a model in `~/.mirrin/tts` gets the on-device detector instead, which is a beat quicker: Mirrin's persona names `hey_mirrin.onnx`, so one you train is used once it is there. To give another name a detector, train an openWakeWord model as in `docs/wake-word.md` and point `wake_model` at it.

## Writing a good one

Write the character as you would brief an actor: a paragraph of temperament and relationship, one line on what it never does. Keep style rules to three or four. Choose acknowledgements the voice can pronounce. Test out loud; a persona that reads well on paper can grate at 8am.
