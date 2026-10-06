package voice

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// speechQueue speaks sentences as they are pushed: synthesis runs one sentence
// ahead of playback, so the first words start while the model is still
// writing the rest. One queue is active at a time; Interrupt cancels it.
type speechQueue struct {
	c      *Channel
	ctx    context.Context
	cancel context.CancelFunc
	in     chan string
	done   chan struct{}
	seq    int

	onFirstAudio func()
	firstSynth   time.Duration

	tmu  sync.Mutex
	text string // everything pushed so far, to tell our own words from the owner's
}

func (c *Channel) newSpeechQueue(parent context.Context) *speechQueue {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	q := &speechQueue{c: c, ctx: ctx, cancel: cancel, in: make(chan string, 64), done: make(chan struct{})}
	go q.run()
	return q
}

// Push adds a sentence.
func (q *speechQueue) Push(sentence string) {
	sentence = speakable(sentence)
	if sentence == "" {
		return
	}
	q.tmu.Lock()
	q.text = strings.TrimSpace(q.text + " " + sentence)
	q.c.saying.Store(q.text)
	q.tmu.Unlock()
	select {
	case q.in <- sentence:
	case <-q.ctx.Done():
	}
}

// Finish closes the queue and waits for everything to be spoken.
func (q *speechQueue) Finish() {
	close(q.in)
	<-q.done
}

func (q *speechQueue) run() {
	c := q.c
	defer close(q.done)
	c.speakMu <- struct{}{}
	defer func() { <-c.speakMu }()
	c.smu.Lock()
	c.speaking = true
	c.speakCancel = q.cancel
	c.smu.Unlock()
	c.setState("speaking")
	defer func() {
		q.cancel()
		c.smu.Lock()
		c.speaking, c.player, c.paused, c.speakCancel = false, nil, false, nil
		c.smu.Unlock()
		c.setState("idle")
	}()
	if c.wake != nil {
		c.wake.send("speaking")
		defer c.wake.send("idle")
	}

	if c.eleven == nil && c.kokoro == nil {
		for s := range q.in {
			if q.ctx.Err() != nil {
				return
			}
			c.speakSystem(q.ctx, s)
		}
		return
	}

	// Synthesis stage: one sentence ahead of playback.
	clips := make(chan clip, 1)
	go func() {
		defer close(clips)
		for s := range q.in {
			if q.ctx.Err() != nil {
				return
			}
			for _, cl := range c.synthesize(q.ctx, s, &q.seq) {
				select {
				case clips <- cl:
				case <-q.ctx.Done():
					return
				}
			}
		}
	}()

	// Playback stage: one continuous PCM stream to the speakers, so the first
	// sentence starts as soon as it is decoded and later ones follow without gaps.
	var out *pcmPlayer
	defer func() {
		if out != nil {
			out.finish()
		}
	}()
	direct := false // no player: the system voice says the text instead
	first := true
	for cl := range clips {
		if q.ctx.Err() != nil {
			return
		}
		if first {
			first = false
			if q.onFirstAudio != nil {
				q.onFirstAudio()
			}
			q.firstSynth = cl.synth
		}
		if cl.err != nil {
			// Every voice that writes a file failed: the system voice speaks it.
			fmt.Fprintf(c.out, "(voice: %v)\n", cl.err)
			c.speakSystem(q.ctx, cl.text)
			continue
		}
		if !direct && out == nil {
			p, err := startPCMPlayer(q.ctx)
			if err != nil {
				c.problem(troublePlayer, fmt.Sprintf("I can't play my voice (%s), so I'm using the plain system voice. To get it back: %s.", brief(err), installHint("sox")))
				direct = true
			} else {
				out = p
				c.smu.Lock()
				c.player = p.cmd
				c.smu.Unlock()
			}
		}
		if direct {
			c.speakSystem(q.ctx, cl.text)
			continue
		}
		err := out.playFile(q.ctx, cl.path)
		switch {
		case err == nil:
			c.recovered(troublePlayer)
		case q.ctx.Err() == nil:
			fmt.Fprintf(c.out, "(play: %v)\n", err)
			c.problem(troublePlayer, fmt.Sprintf("Playing my voice failed (%s), so I'm using the plain system voice for now.", brief(err)))
			direct = true
			c.speakSystem(q.ctx, cl.text)
		}
	}
}

// pcmPlayer is a long-lived audio output fed with raw 24 kHz mono PCM.
type pcmPlayer struct {
	cmd *exec.Cmd
	in  io.WriteCloser
}

func startPCMPlayer(ctx context.Context) (*pcmPlayer, error) {
	if _, err := exec.LookPath("sox"); err != nil {
		return nil, fmt.Errorf("sox is required for playback")
	}
	cmd := exec.CommandContext(ctx, "sox", "-q", "-t", "raw", "-r", "24000", "-c", "1", "-b", "16", "-e", "signed", "-L", "-", "-d")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &pcmPlayer{cmd: cmd, in: in}, nil
}

// playFile decodes a clip to raw PCM and streams it into the player.
func (p *pcmPlayer) playFile(ctx context.Context, path string) error {
	dec := exec.CommandContext(ctx, "sox", "-q", path, "-t", "raw", "-r", "24000", "-c", "1", "-b", "16", "-e", "signed", "-L", "-")
	data, err := dec.Output()
	if err != nil {
		return err
	}
	_, err = p.in.Write(data)
	return err
}

// finish closes the stream and waits for the last samples to play.
func (p *pcmPlayer) finish() {
	_ = p.in.Close()
	_ = p.cmd.Wait()
}

// sentenceBuffer turns a stream of text fragments into complete sentences.
type sentenceBuffer struct {
	buf     strings.Builder
	emitted int
}

// firstClauseAfter is how long the first sentence may grow before we speak
// its opening clause early (cut at a comma or dash) to get sound out sooner.
const firstClauseAfter = 45

// reAbbrevEnd matches text that ends in an abbreviation or a list marker, where a
// full stop is not the end of a sentence ("e.g.", "Mr.", "1.").
var reAbbrevEnd = regexp.MustCompile(`(?i)(?:^|\s)(?:e\.g|i\.e|vs|mr|mrs|ms|dr|st|no|approx|jan|feb|mar|apr|jun|jul|aug|sep|sept|oct|nov|dec)\.$|^\s*\d{1,2}[.)]$`)

// boundary finds the first real sentence boundary in text: punctuation
// followed by whitespace that isn't an abbreviation, or a line break. It
// returns where the sentence ends and where the rest starts, or -1.
func boundary(text string) (cut, next int, newline bool) {
	cut, next = -1, -1
	for _, idx := range reSentence.FindAllStringIndex(text, -1) {
		if !isSpace(text[idx[1]-1]) {
			continue // punctuation must be followed by whitespace ("3.5", "e.g." mid-stream)
		}
		if reAbbrevEnd.MatchString(strings.TrimSpace(text[:idx[1]])) {
			continue
		}
		cut, next = idx[1], idx[1]
		break
	}
	if nl := strings.IndexByte(text, '\n'); nl >= 0 && (cut < 0 || nl < cut-1) {
		return nl, nl + 1, true
	}
	return cut, next, false
}

// Add appends a fragment and returns any sentences that are now complete.
func (b *sentenceBuffer) Add(delta string) []string {
	b.buf.WriteString(delta)
	text := b.buf.String()
	var out []string
	for {
		cut, next, nl := boundary(text)
		if cut < 0 {
			break // no confirmed boundary yet
		}
		part := strings.TrimSpace(text[:cut])
		if part != "" {
			if nl && !strings.ContainsAny(part[len(part)-1:], ".!?…:;,") {
				part += "." // a line break is a pause, so let the voice treat it as one
			}
			out = append(out, part)
		}
		text = text[next:]
	}
	// Nothing complete yet and the first sentence is running long: speak its first clause.
	if len(out) == 0 && b.emitted == 0 && len(text) > firstClauseAfter {
		if i := clauseCut(text); i > 0 {
			out = append(out, strings.TrimSpace(text[:i]))
			text = strings.TrimSpace(text[i:])
		}
	}
	b.buf.Reset()
	b.buf.WriteString(text)
	out = mergeShort(out)
	b.emitted += len(out)
	return out
}

// clauseCut finds a dash or semicolon boundary after the first 12 chars.
// Commas are deliberately not cut points: a clause spoken on its own after a
// comma gets the wrong intonation and sounds like a different sentence.
func clauseCut(text string) int {
	best := -1
	for _, sep := range []string{" — ", "; ", " – "} {
		if len(text) > 12 && strings.Contains(text[12:], sep) {
			i := strings.Index(text[12:], sep)
			pos := 12 + i + len(sep)
			if best < 0 || pos < best {
				best = pos
			}
		}
	}
	return best
}

// Flush returns whatever is left.
func (b *sentenceBuffer) Flush() []string {
	rest := strings.TrimSpace(b.buf.String())
	b.buf.Reset()
	if rest == "" {
		return nil
	}
	return []string{rest}
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\t' }

func mergeShort(parts []string) []string {
	var merged []string
	for _, p := range parts {
		if n := len(merged); n > 0 && len(merged[n-1]) < 25 {
			merged[n-1] += " " + p
			continue
		}
		merged = append(merged, p)
	}
	return merged
}

// voiceStream is the channel's incremental reply: sentences are spoken as
// soon as they are complete.
type voiceStream struct {
	c                *Channel
	q                *speechQueue
	buf              sentenceBuffer
	text             strings.Builder
	once             sync.Once
	wrote            bool
	approvalID       int64
	approvalRevision uint64
	askedByHand      bool
	approvalDirty    bool
	approvalPrompt   string
	approvalBuf      strings.Builder
	t                timing
	// steps counts the tool steps so far. Once one has run, words are held
	// (held) instead of spoken: what the twin writes between steps is its
	// working ("didn't click add to cart yet, the tooltip…"), and only the
	// words after the last step, the result, are said.
	steps int
	held  strings.Builder
}

// OpenStream starts speaking a reply as it is generated.
func (c *Channel) OpenStream(ctx context.Context, _ string) channelsStream {
	v := &voiceStream{c: c, t: c.takeTiming(), approvalDirty: true}
	if c.ApprovalRevision != nil {
		v.approvalRevision = c.ApprovalRevision()
	}
	if c.ApprovalPrompt != nil {
		v.approvalID, _ = c.ApprovalPrompt()
	}
	v.q = c.newSpeechQueue(ctx)
	v.q.onFirstAudio = func() {
		if v.t.firstAudio.IsZero() {
			v.t.firstAudio = time.Now()
		}
	}
	return v
}

// refreshApproval queries only when approval events advance the cached
// revision. Standalone channels without events fall back to tool captions,
// which precede execution; the next delta follows the newly raised request.
func (v *voiceStream) refreshApproval() {
	if v.c.ApprovalRevision != nil {
		if revision := v.c.ApprovalRevision(); revision != v.approvalRevision {
			v.approvalRevision, v.approvalDirty = revision, true
		}
	}
	if !v.approvalDirty {
		return
	}
	v.approvalDirty = false
	if v.c.ApprovalPrompt == nil {
		return
	}
	if id, prompt := v.c.ApprovalPrompt(); id > v.approvalID && prompt != "" {
		v.approvalID = id
		v.approvalPrompt = prompt
		v.askedByHand = false
		for _, s := range v.buf.Flush() {
			v.approvalBuf.WriteString(s)
			v.approvalBuf.WriteByte(' ')
		}
	}
}

func (v *voiceStream) Write(delta string) {
	if v.steps > 0 {
		if !v.wrote && v.t.firstDelta.IsZero() {
			v.t.firstDelta = time.Now()
		}
		v.held.WriteString(delta)
		v.wrote = true
		return
	}
	v.say(delta)
}

func (v *voiceStream) say(delta string) {
	v.refreshApproval()
	if !v.wrote && v.t.firstDelta.IsZero() {
		v.t.firstDelta = time.Now()
	}
	if v.approvalPrompt == "" {
		for _, s := range v.buf.Add(delta) {
			v.speakSentence(s)
		}
	} else {
		// Keep complete sentences while filtering approval questions: the
		// normal buffer can merge short sentences or emit an early clause.
		v.approvalBuf.WriteString(delta)
		v.flushApproval(false)
	}
	v.wrote = true
}

func (v *voiceStream) speakSentence(s string) {
	if v.approvalPrompt != "" && asksForAnswer(s) {
		v.speakApproval()
		return
	}
	v.q.Push(s)
	v.text.WriteString(strings.TrimSpace(s) + " ")
}

func (v *voiceStream) speakApproval() {
	if v.approvalPrompt != "" && !v.askedByHand {
		v.q.Push(v.approvalPrompt)
		v.text.WriteString(v.approvalPrompt + " ")
		v.askedByHand = true
	}
}

func (v *voiceStream) flushApproval(final bool) {
	text := v.approvalBuf.String()
	for {
		cut, next, _ := boundary(text)
		if cut < 0 {
			break
		}
		if s := strings.TrimSpace(text[:cut]); s != "" {
			v.speakSentence(s)
		}
		text = text[next:]
	}
	v.approvalBuf.Reset()
	if final {
		if s := strings.TrimSpace(text); s != "" {
			v.speakSentence(s)
		}
	} else {
		v.approvalBuf.WriteString(text)
	}
}

// Note hears a step's caption ("Opening the page"). It isn't spoken: the
// owner hears the answer, not each step on the way (the screen still shows
// them). What was said before the step is spoken first, in order.
func (v *voiceStream) Note(text string) {
	if v.c.ApprovalRevision == nil {
		v.approvalDirty = true
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	// Flush any pending holding line first so the order stays natural.
	for _, s := range v.buf.Flush() {
		v.speakSentence(s)
	}
	v.flushApproval(true)
	if v.steps == 0 && v.text.Len() == 0 && v.t.ack.IsZero() && v.approvalPrompt == "" {
		// Work is starting and nothing has been said: a person says "Sure,
		// one sec" at once rather than leaving a silence.
		nod := workNods[workNod.Add(1)%uint32(len(workNods))]
		v.q.Push(nod)
		v.text.WriteString(nod + " ")
	}
	v.steps++
	v.held.Reset() // words between steps were working, not the answer
	fmt.Fprintf(v.c.out, "  … %s\n", text)
}

// workNods are said, in turn, when a reply needs a step before any words.
var workNods = []string{"Sure, one moment.", "On it.", "Right, give me a second.", "Sure."}

var workNod atomic.Uint32

func (v *voiceStream) Close() {
	v.once.Do(func() {
		if v.held.Len() > 0 { // the words after the last step: the result
			v.say(v.held.String())
		}
		v.refreshApproval()
		for _, s := range v.buf.Flush() {
			v.speakSentence(s)
		}
		v.flushApproval(true)
		v.speakApproval()
		if t := strings.TrimSpace(v.text.String()); t != "" {
			fmt.Fprintf(v.c.out, "%s: %s\n", v.c.name, t)
			v.c.lastSaid.Store(t)
		}
		v.q.Finish()
		v.t.done = time.Now()
		v.t.synth = v.q.firstSynth
		v.c.reportTiming(v.t)
	})
}
