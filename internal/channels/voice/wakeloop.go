package voice

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// wakeLoop: on-device detector, barge-in, follow-ups.
//
// The helper's events are handled asynchronously so a wake word can pause
// playback the instant it fires, and the transcript can then either confirm
// it (stop talking, act) or reject it (resume talking).
//
// The twin only speaks once the wake phrase is confirmed: the chime on the
// detector's first stage says "heard something", and the spoken
// acknowledgement ("Right.") waits for whisper to confirm the name, so a
// false wake from the television gets no "Hello." to the room. To keep that
// acknowledgement quick, the helper hands over the wake phrase on its own
// the moment it fires, and it is confirmed while the owner is still talking;
// when they finish, the acknowledgement plays at once.
func (c *Channel) wakeLoop(ctx context.Context, handler channels.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	confirmed := make(chan string, 4) // wake-confirmed commands
	followups := make(chan string, 4) // replies during a follow-up window ("" = nothing)
	stopped := make(chan error, 1)    // the helper couldn't be kept running
	c.wake.onWake = func() { c.pausePlayback() }
	c.wakeLive.Store(true)
	defer c.wakeLive.Store(false)
	if err := c.wake.start(ctx); err != nil {
		return err
	}
	c.prepareAcks(ctx)
	defer c.wake.stop()

	go c.wakeEvents(ctx, confirmed, followups, stopped)

	followup := func() string {
		c.setState("listening")
		defer c.setState("idle")
		c.followupOpen.Store(true)
		defer c.followupOpen.Store(false)
		if !sleepCtx(ctx, 300*time.Millisecond) { // let the speaker's tail die away
			return ""
		}
		for len(followups) > 0 { // drop stale results from an earlier window
			<-followups
		}
		c.wake.send("listen")
		timeout := time.NewTimer(time.Duration(c.cfg.FollowupSeconds+c.cfg.MaxSeconds+5) * time.Second)
		defer timeout.Stop()
		select {
		case <-ctx.Done():
			return ""
		case <-timeout.C:
			return ""
		case t := <-followups:
			if t == "" {
				fmt.Fprintf(c.out, "(follow-up: nothing heard)\n")
			}
			return t
		case t := <-confirmed: // the user said the wake word instead; treat it as the follow-up
			return t
		}
	}

	for {
		var rest string
		select {
		case <-ctx.Done():
			return nil
		case err := <-stopped:
			return err
		case rest = <-confirmed:
		case <-c.listenNow:
			// A click on the orb: listen at once, silently. The orb turning
			// green is the answer; nobody said anything to reply to.
			if rest = followup(); rest == "" {
				continue
			}
		}
		if rest == "" {
			c.Speak(ctx, c.bareWake()) // "Sir?", "Akshaya?" (acks.go)
			rest = followup()
			if rest == "" {
				continue
			}
		}
		c.converse(ctx, handler, rest, followup)
	}
}

// wakeRound is one firing of the detector: whether its wake phrase has been
// confirmed from the pre-roll clip already, and the words it was heard as.
type wakeRound struct {
	id        uint64
	confirmed atomic.Bool
	phrase    atomic.Value // string: the wake phrase as the clip's transcript has it
}

// clipPhrase is the confirmed wake phrase ("hey mirrin"), or "".
func (r *wakeRound) clipPhrase() string {
	s, _ := r.phrase.Load().(string)
	return s
}

// helperRestarts is how many times in a row a helper that stopped is
// started again, waiting restartBackoff, then twice that, and so on.
const helperRestarts = 3

var restartBackoff = time.Second

// wakeEvents handles the helper's events until ctx ends.
func (c *Channel) wakeEvents(ctx context.Context, confirmed, followups chan string, stopped chan error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := 0
	round := &wakeRound{}
	var latestConfirmed uint64
	results := make(chan wakeTranscript, 8)
	restarts, lastRestart := 0, time.Time{}
	lastErr := ""
	ackOn := c.cfg.Acknowledge == nil || *c.cfg.Acknowledge
	for {
		var ev wakeEvent
		select {
		case <-ctx.Done():
			return
		case ev = <-c.wake.events:
		case result := <-results:
			if result.round.id < latestConfirmed {
				continue // only a confirmed newer wake supersedes an older command
			}
			if result.err == nil {
				if rest, ok := stripWakeWith(result.text, c.cfg.WakeWord, c.wakeAliases); ok {
					result.round.phrase.Store(wakeWords(result.text, rest))
					result.round.confirmed.Store(true)
				}
				if result.round.confirmed.Load() {
					latestConfirmed = result.round.id
				}
			}
			if result.clip {
				if c.clipChecked != nil {
					c.clipChecked()
				}
				continue
			}
			if result.ev.followup {
				c.onFollowupTranscript(ctx, result, confirmed, followups)
			} else {
				c.onWakeTranscript(ctx, result, ackOn, confirmed, followups)
			}
			continue
		}
		switch ev.kind {
		case "interrupt":
			round = &wakeRound{id: round.id + 1}
			fmt.Fprintf(c.out, "(someone's talking over me)\n")
			c.pausePlayback()
		case "wake":
			fmt.Fprintf(c.out, "(wake %s)\n", ev.arg)
			c.setState("listening")
			round = &wakeRound{id: round.id + 1}
			if !c.isSpeaking() && ackOn {
				go c.chime() // a soft tick: heard something, keep talking
			}
			// Quiet on purpose: the user is usually still mid-sentence. Words
			// come once the wake phrase is confirmed (see "utterance").
		case "wakeclip":
			// The wake phrase alone: confirm it while the user is still talking.
			// Only with the model loaded: two whisper-cli runs at once would
			// slow down the one that matters on a slower machine.
			if !c.quickEars() {
				continue
			}
			r := round
			path, cleanup, err := c.snapshotCapture(ev.arg)
			if err != nil {
				continue
			}
			go func(path string) {
				defer cleanup()
				text, err := c.Transcribe(ctx, path)
				select {
				case results <- wakeTranscript{round: r, text: text, err: err, clip: true}:
				case <-ctx.Done():
				}
			}(path)
		case "near":
			fmt.Fprintf(c.out, "(near-miss wake %s)\n", ev.arg)
		case "utterance":
			failures = 0
			result := wakeTranscript{ev: ev, round: round, started: time.Now()}
			c.setState("thinking")
			if !ev.followup && round.confirmed.Load() && !ev.bare && ackOn && !c.isSpeaking() {
				result.ack = time.Now()
				hello := time.Since(c.lastExchange) > 10*time.Minute
				go c.acknowledge(hello)
			}
			path, cleanup, captureErr := c.snapshotCapture(ev.arg)
			go func(workCtx context.Context, result wakeTranscript) {
				defer cleanup()
				result.err = captureErr
				if result.err == nil {
					result.text, result.err = c.Transcribe(workCtx, path)
				}
				select {
				case results <- result:
				case <-workCtx.Done():
				}
			}(ctx, result)
		case "silence":
			c.resumePlayback()
			c.setState("idle")
			if ev.followup {
				select {
				case followups <- "":
				default:
				}
				continue
			}
			failures++
			if c.OnNoAudio != nil && failures >= 3 {
				c.OnNoAudio(failures)
			}
		case "mute":
			c.problem(troubleMic, "I'm not getting any sound from the microphone. If you just allowed microphone access, restart me from the menu; otherwise check the input device in Sound settings.")
		case "unmute":
			c.recovered(troubleMic)
		case "err":
			if !c.wake.current(ev) {
				continue // from a helper already replaced
			}
			fmt.Fprintf(c.out, "(wake: %s)\n", ev.arg)
			if !strings.Contains(ev.arg, "exited") {
				lastErr = ev.arg
				continue
			}
			// The helper stopped (the microphone went away, sox or Python
			// crashed): start it again a few times before giving up.
			if time.Since(lastRestart) > 10*time.Minute {
				restarts = 0
			}
			reason := lastErr
			restarted := false // in this pass: a helper that restarted and died again counts
			for restarts < helperRestarts {
				restarts++
				lastRestart = time.Now()
				if !sleepCtx(ctx, restartBackoff<<(restarts-1)) {
					return
				}
				c.wake.stop()
				if err := c.wake.start(ctx); err != nil {
					reason = brief(err)
					continue
				}
				fmt.Fprintf(c.out, "(wake helper restarted)\n")
				lastErr, restarted = "", true
				break
			}
			if restarted {
				continue
			}
			c.wake.stop()
			why := ""
			if reason != "" {
				why = " (" + reason + ")"
			}
			c.problem(troubleMic, fmt.Sprintf("I've stopped listening for \"Hey %s\": my microphone helper keeps stopping%s. Check the microphone is plugged in, then restart me from the menu; if it happens again, run mirrin doctor.", titleWords(c.cfg.WakeWord), why))
			if reason == "" {
				reason = "it kept stopping"
			}
			select {
			case stopped <- fmt.Errorf("the wake-word helper stopped: %s", reason):
			default:
			}
			return
		}
	}
}

// onWakeUtterance handles what was said after the detector fired: whisper
// must confirm the wake phrase before the twin says a word.
type wakeTranscript struct {
	ev           wakeEvent
	round        *wakeRound
	started, ack time.Time
	text         string
	err          error
	clip         bool
}

func (c *Channel) onWakeTranscript(ctx context.Context, result wakeTranscript, ackOn bool, confirmed, followups chan string) {
	round := result.round
	t0, ackAt := result.started, result.ack
	text, err := result.text, result.err
	hello := time.Since(c.lastExchange) > 10*time.Minute
	ack := func() {
		if ackAt.IsZero() && ackOn && !c.isSpeaking() {
			ackAt = time.Now()
			go c.acknowledge(hello)
		}
	}
	if err != nil {
		fmt.Fprintf(c.out, "(transcribe: %v)\n", err)
		c.resumePlayback()
		c.setState("idle")
		return
	}
	// Second stage: the capture includes the wake phrase, so whisper must
	// confirm it. This is what stops "the Mirrin pilot" or babble waking it.
	rest, ok := stripWakeWith(text, c.cfg.WakeWord, c.wakeAliases)
	if !ok && round.confirmed.Load() {
		// Its own clip confirmed the name (and it may have been acknowledged
		// already); the whole utterance just spells it another way ("Hey
		// Mirrins, …"). Going quiet now would be the worst answer.
		rest, ok = afterWakePhrase(text, round.clipPhrase(), c.cfg.WakeWord, c.wakeAliases), true
	}
	if !ok {
		if c.followupOpen.Load() && (!hallucinated(text) || shortAnswer(text, c.names()...) && c.expectingAnswer()) {
			// Spoken inside a follow-up window: it's the reply, not a wake attempt.
			c.resumePlayback()
			select {
			case followups <- strings.TrimSpace(text):
			default:
			}
			return
		}
		fmt.Fprintf(c.out, "(ignored, wake phrase not confirmed: %q)\n", text)
		c.resumePlayback()
		c.setState("idle")
		return
	}
	rest = stripAck(rest, c.bareWake())
	if hallucinated(rest) && strings.Contains(rest, "*") {
		fmt.Fprintf(c.out, "(ignored non-speech: %q)\n", rest)
		c.resumePlayback()
		c.setState("idle")
		return
	}
	if c.stopTurn(rest) {
		return
	}
	if c.handling.Load() || result.ack.IsZero() && c.isSpeaking() {
		c.interruptTurn(rest)
	}
	c.lastExchange = time.Now()
	if rest != "" {
		ack() // a bare "Hey Mirrin" gets "Sir?" (bareWake) instead
	}
	c.tmu.Lock()
	c.pending = timing{utterance: t0, ack: ackAt, transcribed: time.Now()}
	c.tmu.Unlock()
	deliverLatest(confirmed, rest)
}

// onFollowupUtterance handles speech heard without the wake word: in a
// follow-up window, or talking over the twin.
func (c *Channel) onFollowupTranscript(ctx context.Context, result wakeTranscript, confirmed, followups chan string) {
	t0 := result.started
	text, err := result.text, result.err
	if err != nil {
		fmt.Fprintf(c.out, "(transcribe: %v)\n", err)
		c.resumePlayback()
		select {
		case followups <- "":
		default:
		}
		return
	}
	c.tmu.Lock()
	c.pending = timing{utterance: t0, transcribed: time.Now()}
	c.tmu.Unlock()
	speaking := c.isSpeaking()
	if (speaking || c.handling.Load()) && isStop(text, c.names()...) && !c.isEcho(text) {
		// A real stop cancels the work too. Its own "Hold on" or "Wait",
		// heard back from the speaker, isn't one.
		fmt.Fprintf(c.out, "(stopped: %q)\n", text)
		c.wake.send("confirm")
		c.interruptTurn(text)
		return
	}
	// A one-word "yes" looks like whisper's guesses at noise, but when the
	// twin has just asked something (or an approval is waiting) it is the
	// answer, and must never be thrown away.
	answer := !speaking && shortAnswer(text, c.names()...) && c.expectingAnswer()
	// Only sound caught while the speaker could be playing can be the twin's
	// own voice. A follow-up window opens after the reply has finished, so
	// words from the reply heard there are the owner answering it ("Which
	// one, the comfortable option or…?" "Comfortable option.").
	echoable := speaking || !result.ev.followup
	if !answer && !speaking && result.ev.followup && closing(text) {
		// "Okay." "Thanks." after a reply: the owner is done, as a person
		// would take it. Nothing to answer; the window just closes.
		fmt.Fprintf(c.out, "(you: %q, done for now)\n", strings.TrimSpace(text))
		select {
		case followups <- "":
		default:
		}
		return
	}
	if !answer && (hallucinated(text) || (echoable && c.isEcho(text)) || repetitive(text) || (speaking && c.novelWords(text) < 3)) {
		if text != "" {
			fmt.Fprintf(c.out, "(ignored, that was me: %q)\n", text)
		}
		text = ""
	}
	text, _ = stripWakeLoose(text, c.cfg.WakeWord)
	if speaking {
		// An interruption without the wake word: real speech stops the reply
		// and becomes the next turn; noise just resumes playback.
		if text == "" {
			c.wake.send("reject") // teach the helper how loud our own voice gets
			c.resumePlayback()
			return
		}
		c.wake.send("confirm")
		c.interruptTurn(text)
		deliverLatest(confirmed, text)
		return
	}
	select {
	case followups <- text:
	default:
	}
}

// stopTurn cuts short the turn being answered when what was said after the
// wake phrase is a stop ("Hey Mirrin, stop"). It reports whether it did:
// the stop then needs no turn of its own, and the stopped turn says so.
func (c *Channel) stopTurn(rest string) bool {
	if (!c.handling.Load() && !c.isSpeaking()) || (c.OnStop == nil && c.OnInterrupt == nil) || !isStop(rest, c.names()...) {
		return false
	}
	fmt.Fprintf(c.out, "(stop: %q)\n", rest)
	c.interruptTurn(rest)
	return true
}

// quickEars reports whether a transcription costs no model load: the
// whisper server is up (or a test stands in for whisper).
func (c *Channel) quickEars() bool {
	return c.transcribeFn != nil || c.whisper != nil && c.whisper.running()
}

// sleepCtx waits d, or less if ctx ends first (then it returns false).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// wakeWords is the wake phrase as a transcript has it: its words before the
// rest that stripWake left ("Hey Mirrin." → "hey mirrin").
func wakeWords(text, rest string) string {
	all, after := strings.Fields(words(text)), strings.Fields(words(rest))
	n := len(all) - len(after)
	if n <= 0 {
		return ""
	}
	return strings.Join(all[:n], " ")
}

// reToken finds the words of a transcript, and what follows each.
var reToken = regexp.MustCompile(`[\p{L}\p{N}'’]+`)

// afterWakePhrase is what was asked, from an utterance whose wake phrase
// was confirmed by its own clip but is spelled differently in the whole
// transcript ("Hey Mirrins, remind me…"). Leading words that sound like
// the phrase go ("Mirrins" for "Mirrin", "Mir" and a stray "in"
// before a comma); if nothing sounds like it, the transcript is kept whole,
// since the twin copes with a garbled name better than with silence.
func afterWakePhrase(text, phrase, wake string, extra []string) string {
	text = strings.TrimSpace(text)
	var refs []string
	for _, s := range append([]string{phrase, wake}, extra...) {
		for _, w := range strings.Fields(words(s)) {
			if utf8.RuneCountInString(w) >= 2 {
				refs = append(refs, w)
			}
		}
	}
	n := len(strings.Fields(words(phrase)))
	if n == 0 {
		n = len(strings.Fields(words(wake))) + 1 // "hey <name>"
	}
	sounds := func(tok string) bool {
		tok = words(tok)
		for _, r := range refs {
			if soundsLike(tok, r) {
				return true
			}
		}
		return false
	}
	cut := -1
	toks := reToken.FindAllStringIndex(text, -1)
	for i, span := range toks {
		if i > n { // the phrase plus one stray word at most
			break
		}
		if sounds(text[span[0]:span[1]]) {
			cut = span[1]
		}
		// A comma or full stop ends the phrase once part of it was heard.
		next := len(text)
		if i+1 < len(toks) {
			next = toks[i+1][0]
		}
		if cut >= 0 && strings.ContainsAny(text[span[1]:next], ",.!?;:") {
			cut = next
			break
		}
	}
	if cut < 0 {
		return text
	}
	return strings.TrimSpace(reWake.ReplaceAllString(text[cut:], ""))
}

// soundsLike reports whether a heard word is a near miss for a wake word:
// the same, one a prefix of the other ("mirrins", "mir"), or sharing
// their first four letters ("mirron").
func soundsLike(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) >= 3 && len(rb) >= 3 && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a)) {
		return true
	}
	same := 0
	for same < len(ra) && same < len(rb) && ra[same] == rb[same] {
		same++
	}
	return same >= 4
}

// deliverLatest keeps wake handling independent of a busy conversation.
func deliverLatest(ch chan string, text string) {
	select {
	case ch <- text:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- text:
	default:
	}
}

// closing reports whether words heard after a reply end the conversation
// ("Okay.", "Thanks.", "Cool, thank you.").
func closing(text string) bool {
	switch strings.Trim(strings.ToLower(strings.TrimSpace(text)), ".,!? ") {
	case "okay", "ok", "thanks", "thank you", "cool", "great", "perfect", "got it", "cheers", "okay thanks", "okay thank you", "ok thanks", "cool thanks", "cool thank you", "great thanks", "great thank you", "perfect thanks", "perfect thank you":
		return true
	}
	return false
}
