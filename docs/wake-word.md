# Wake words

Every persona hears its name on your computer, in one of two ways:

- **With a wake-word model** for the name, openWakeWord listens on the
  device and nothing is transcribed until it hears the name.
- **Without one**, whisper transcribes short stretches of speech locally and
  the name is matched in the text. This works for any name and is a beat
  slower.

Mirrin ships no wake-word model yet. Until the licence-clean models below
are released, the default persona, Mirrin, hears "Hey Mirrin" the way every
other persona without a model hears its name: in what is transcribed.
`mirrin voice setup` and the Health page say so in a line. You can train a
model of your own (below), and a model already on your computer keeps being
used.

## Licence-clean models (trained, not released yet)

Work package WP-24 rebuilt the training pipeline from permissively licensed
inputs only ([tools/wakeword](../tools/wakeword/README.md)) and trained
"Hey Mirrin", "Hey Nyra" and "Hey Pickoo". They are not shipped or embedded
yet: releasing them waits on the owner's real-voice check and the open
caveats below.

What went in, and under which licence (the full inventory, with checksums,
is `tools/wakeword/DATA-LICENCES.md`):

| Ingredient | Licence | Used for |
| --- | --- | --- |
| Kokoro-82M v1.0 and 40 of its voices | Apache-2.0 | spoken wake phrases, near-misses and other speech |
| Piper voices: LibriTTS (904 speakers) | CC BY 4.0 | the same |
| Piper voices: LJ Speech, Kristin, John, Norman, Cori | MIT (trained on public-domain recordings) | the same |
| LibriSpeech train-clean-100, dev, test | CC BY 4.0 | ordinary speech that must not wake it |
| MUSAN noise, music and speech, only files under public domain, CC0 or CC BY | CC BY 4.0 | background sound and negatives |
| Simulated room responses (OpenSLR 28) | Apache-2.0 | reverberation |
| openWakeWord 0.5.1 melspectrogram and embedding models | Apache-2.0 | the frozen front-end every model runs on |
| Google speech_embedding/1 | Apache-2.0 | evidence for where the embedding's weights come from |

Left out on purpose: openWakeWord's ACAV100M features and every other
precomputed feature set (CC BY-NC-SA), macOS `say` voices (Apple's terms),
Piper voices with unclear lineage (lessac and voices fine-tuned from it), the
real recorded room responses in OpenSLR 28, MUSAN files under BY-SA, BY-NC or
other terms, and Common Voice, whose downloads now sit behind an account and
terms gate.

Voices, speakers, recordings and rooms are split into training, calibration
and test sets before anything is synthesised. The results below come from the
test set only: 8 Kokoro voices, the Piper "norman" voice and 75 LibriTTS
speakers never heard in training, and 28.6 hours of held-out LibriSpeech and
MUSAN audio for false accepts. A false accept is a detection more than 1.5
seconds after the last one, as at runtime. The target is recall of at least
0.8 with no more than 0.5 false accepts per hour.

| Model | Recall, clean | Recall, with noise and rooms | False accepts per hour | Near-misses accepted | Target |
| --- | --- | --- | --- | --- | --- |
| Hey Mirrin | 0.99 | 0.96 | 0.14 | 34% | met |
| Hey Nyra | 0.99 | 0.975 | 0.32 | 16% | met |
| Hey Pickoo | 0.99 | 0.97 | 0.95 | 25% | not met |

All three at threshold 0.95, chosen on calibration data, with 360 test
phrases per model. The 95% upper bound on false accepts is 0.36 an hour for
Mirrin, 0.60 for Nyra and 1.38 for Pickoo, so Nyra's margin is thin. Nyra
needed a second training run that adds mined hard negatives from the training
data. Pickoo's second run ended on an early checkpoint that is far quieter
(0.07 false accepts an hour) but hears only 91% of wake phrases, so the first
Pickoo model is the one kept, and it still fires too often, mostly on read
speech. Near-misses are the weak spot for all
three: "hey mirror", "hey Tyra" and "hey pickle" often wake them. The details
are in `tools/wakeword/README.md`.

**Recall here is measured on synthetic voices only.** No real person's voice
was in training or testing. The deciding test is the owner's own voice:
`tools/wakeword/record_holdout.sh` records wake phrases, near-misses and a few
minutes of ordinary talk, and `pipeline/evaluate_holdout.py` scores them. A
model is not released until that passes.

At runtime a detection is never acted on alone: whisper confirms the name in
its own transcript before the twin answers (see "How the detector behaves"),
so a false accept costs a chime, not a reply.

Open caveats for release review:

- openWakeWord's README places its "included pre-trained models" under CC
  BY-NC-SA 4.0. The front-end models are derived from Apache-2.0 work: the
  pipeline checks that the melspectrogram model holds only textbook signal
  processing constants and that every learned weight in the embedding model
  is Google's Apache-2.0 speech_embedding/1. The author hasn't yet confirmed
  in writing that the README wording doesn't cover them
  (dscripka/openWakeWord#348, open).
- Kokoro's model card says its training audio includes synthetic speech made
  with closed commercial text-to-speech services. Kokoro's weights are
  Apache-2.0; whether those services' terms reach its output is the
  publisher's statement to stand behind, not something the pipeline can check.
- Kokoro and Piper turn text into phonemes with espeak-ng (GPL-3.0) while
  synthesising. It is a tool run on our own text; none of its code or data is
  in a model.

## The old "Hey Maverick" model

Earlier builds embedded a "Hey Maverick" model (`hey_maverick.onnx`, about
400 KB) that the maintainers trained with the scripts below, when the default
persona was called Maverick. Its training used:

- openWakeWord's precomputed ACAV100M speech features as negative data. They
  are published under **CC BY-NC-SA 4.0**, which allows no commercial use.
- Positive clips spoken by Kokoro voices and the macOS system voices
  (`say`), whose terms are Apple's.

The clips were synthetic speech, but the model was not "fully synthetic" or
MIT. Whether a model trained on such data inherits those terms is unsettled,
so it is treated as not cleared for commercial use, and it is left out of
the public code and every release. The licence-clean pipeline above uses none
of its data.

A `hey_maverick.onnx` already in your voice folder (`~/.mirrin/tts/`), from
an earlier version, stays there and backups keep it, but it listens for "Hey
Maverick", so Mirrin doesn't use it: he hears "Hey Mirrin" in what is
transcribed until a `hey_mirrin.onnx` is in that folder.

## Train your own (personal, non-commercial use only)

These are the scripts the old model was made with. They use ACAV100M
**CC BY-NC-SA** features, Kokoro and Apple system voices (`say`), so what
they make is **not cleared for commercial use**. Review the input terms and
[licensing notes](licensing.md) before using or sharing a model.

These are editable macOS scripts, not a one-command trainer. They expect
`mirrin voice setup` to have installed Kokoro in `~/.mirrin/tts/`, along with
an environment containing openWakeWord's training dependencies, NumPy and
SoundFile, and the `sox` command. They write clips under `~/.mirrin/wake/`,
or under `$MIRRIN_HOME` when it is set. The config names `~/.mirrin`; change
its paths if you use a different home or a separate training directory.

1. Copy [the config](wake-word-config.yaml) and the scripts below into a working
   directory. Change `model_name`, `target_phrase`, output paths, and the phrase
   and near-miss text in the scripts to match your persona. The defaults train
   "hey maverick", the default persona's name when they were written; for
   Mirrin, change them to "hey mirrin" and name the model `hey_mirrin.onnx`.
2. Run [wake-word-gen_clips.py](wake-word-gen_clips.py) to synthesize positives,
   adversarial negatives, background noise and room responses. Optional additions:
   [gen_extra](wake-word-gen_extra.py) adds voices and rates,
   [gen_adv](wake-word-gen_adv.py) adds targeted near-misses, and
   [gen_mix](wake-word-gen_mix.py) adds overlapping speech.
3. Place openWakeWord's ACAV100M features and validation features at the paths
   named in the config. Augment the clips with `openwakeword.train --augment_clips`,
   then train with `openwakeword.train --train_model`, using your config.
4. Use [wake-word-eval.py](wake-word-eval.py) with your exported `.onnx` path to
   score synthetic phrases, and test separate real recordings before choosing
   `wake_threshold`. Update its phrase text for your custom name too. Keep the
   adversarial negatives: they help prevent phrases like "hey everybody" from
   firing the detector.
5. Put the resulting model in `~/.mirrin/tts/` and select it as described below.

The legacy random splits and mixtures do not provide an independent release
benchmark. Do not reuse these clips or features in the licence-clean pipeline,
which has its own reviewed inputs and held-out test set.

## Use a custom model

Put your openWakeWord `.onnx` model in `~/.mirrin/tts/` (or `$MIRRIN_HOME/tts/`
if you use a custom home). Set `wake_model` in your persona to its bare filename,
for example `hey_nova.onnx`, to have it follow persona changes. Alternatively,
set `channels.voice.wake_model` to its full path, without `~`; that override
stays put across persona changes and restarts. Voice setup installs
openWakeWord even before there is a model; if it said openWakeWord didn't
install, run `mirrin voice setup` again once your model is in place.

Mirrin's persona already names `hey_mirrin.onnx`, so a model you train for
"hey mirrin" works as soon as it is in the voice folder under that name.
Encrypted backups keep the wake-word models in the voice folder, that one
included, so a restore on a new machine brings yours back.

## Building with the old model (maintainers)

A build made with `-tags wakemodel` embeds
`internal/channels/voice/assets/hey_maverick.onnx`, and voice setup puts it
in the voice folder under that name. It listens for "Hey Maverick", so the
default persona, Mirrin, doesn't use it. `make build` and `make install` add the tag only when that file is in the
checkout; the public repository doesn't have it, the release workflow and the
`make dist-*` targets never use the tag, and source archives leave the file
out. Run `go test -tags wakemodel ./internal/channels/voice/` to test that
build.

## How the detector behaves

Historical synthetic tests of the old model, with clean "Hey Maverick" in
held-out voices, reported recall of about **0.8 at threshold 0.35**. Sentences
containing "maverick", such as "the maverick pilot flew fast", can also score
high.

Mirrin therefore confirms a detected wake with Whisper before responding. The
capture includes **1.6 seconds of pre-roll** to preserve the wake phrase. During
playback, the detector uses a lower threshold of **0.1** by default (or the normal
threshold if it is already lower); a detection pauses playback while the transcript
confirms or rejects the wake.

These old scores say nothing about the licence-clean models; their held-out
results are above.

`mirrin wake train` and automatic installation with rollback are still planned.
