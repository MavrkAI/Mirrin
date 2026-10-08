package jev

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Question is one typed question: a Noul, a Choice or a Score.
//
// Instructions and criteria are `any` so a caller can pass a string or a
// structured object or array, which Jev reads as data it may refer to by
// name in backticks. Text written by other people belongs in the state,
// labelled as data, never in instructions.
type Question interface {
	kind() string
	check() error
	wire() wireQuestion
}

// Noul is a yes/no question; the answer is the probability of yes.
type Noul struct {
	Instructions any
	// True and False optionally say what a yes and a no mean.
	True, False any
}

// Choice picks one of Options, in the order given (2 to 255, unique names).
type Choice struct {
	Instructions any
	Options      []Opt
}

// Opt is one option of a Choice: its name, which comes back as the answer,
// and an optional rubric describing it.
type Opt struct {
	Name   string
	Rubric any
}

// Score rates the state on Levels, lowest first (2 to 10).
type Score struct {
	Instructions any
	Levels       []any
}

type wireQuestion struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func (Noul) kind() string   { return "noul" }
func (Choice) kind() string { return "choice" }
func (Score) kind() string  { return "score" }

func (q Noul) check() error { return checkInstructions(q.Instructions) }

func (q Choice) check() error {
	if err := checkInstructions(q.Instructions); err != nil {
		return err
	}
	if len(q.Options) < 2 || len(q.Options) > 255 {
		return fmt.Errorf("%w: a choice needs 2 to 255 options, not %d", ErrInvalid, len(q.Options))
	}
	seen := map[string]bool{}
	for _, o := range q.Options {
		if strings.TrimSpace(o.Name) == "" {
			return fmt.Errorf("%w: an option has no name", ErrInvalid)
		}
		if seen[o.Name] {
			return fmt.Errorf("%w: option %q is there twice", ErrInvalid, o.Name)
		}
		seen[o.Name] = true
	}
	return nil
}

func (q Score) check() error {
	if err := checkInstructions(q.Instructions); err != nil {
		return err
	}
	if len(q.Levels) < 2 || len(q.Levels) > 10 {
		return fmt.Errorf("%w: a score needs 2 to 10 levels, not %d", ErrInvalid, len(q.Levels))
	}
	for i, l := range q.Levels {
		if isBlank(l) {
			return fmt.Errorf("%w: level %d is empty", ErrInvalid, i)
		}
	}
	return nil
}

func checkInstructions(v any) error {
	if isBlank(v) {
		return fmt.Errorf("%w: no instructions", ErrInvalid)
	}
	return nil
}

func isBlank(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

func (q Noul) wire() wireQuestion {
	w := wireQuestion{Type: "noul", Instructions: q.Instructions}
	if q.True != nil || q.False != nil {
		c := map[string]any{}
		if q.True != nil {
			c["true"] = q.True
		}
		if q.False != nil {
			c["false"] = q.False
		}
		w.Criteria = c
	}
	return w
}

func (q Choice) wire() wireQuestion {
	return wireQuestion{Type: "choice", Instructions: q.Instructions, Criteria: orderedOptions(q.Options)}
}

func (q Score) wire() wireQuestion {
	return wireQuestion{Type: "score", Instructions: q.Instructions, Criteria: q.Levels}
}

// orderedOptions is a choice's criteria: a JSON object whose keys keep the
// order the options were given in.
type orderedOptions []Opt

func (o orderedOptions) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, opt := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(opt.Name)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(opt.Rubric)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// wireQuestions checks every question and puts them in the request's shape.
func wireQuestions(qs map[string]Question) (map[string]wireQuestion, error) {
	if len(qs) == 0 {
		return nil, fmt.Errorf("%w: no questions", ErrInvalid)
	}
	out := make(map[string]wireQuestion, len(qs))
	for id, q := range qs {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%w: a question has no id", ErrInvalid)
		}
		if q = deref(q); q == nil {
			return nil, fmt.Errorf("%w: question %q is empty", ErrInvalid, id)
		}
		if err := q.check(); err != nil {
			return nil, fmt.Errorf("question %q: %w", id, err)
		}
		out[id] = q.wire()
	}
	return out, nil
}

type wireAnswer struct {
	Type          string                     `json:"type"`
	Noul          *float64                   `json:"noul"`
	Choice        *string                    `json:"choice"`
	Probabilities map[string]float64         `json:"probabilities"`
	Score         *float64                   `json:"score"`
	Legend        map[string]json.RawMessage `json:"legend"`
	Confidence    *float64                   `json:"confidence"`
}

// decode reads a 200's body and checks that it answers every question in
// qs as asked. Answers to ids that weren't asked are dropped.
func decode(raw []byte, qs map[string]Question) (*Result, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, ErrBadAnswer
	}
	if w.Usage.InputTokens < 0 || w.Usage.OutputTokens < 0 {
		return nil, ErrBadAnswer
	}
	res := &Result{Model: w.Model, Usage: w.Usage, Answers: make(map[string]Answer, len(qs))}
	for id, q := range qs {
		r, ok := w.Answers[id]
		if !ok {
			return nil, ErrBadAnswer
		}
		var wa wireAnswer
		if err := json.Unmarshal(r, &wa); err != nil {
			return nil, ErrBadAnswer
		}
		a, ok := checkAnswer(q, wa)
		if !ok {
			return nil, ErrBadAnswer
		}
		res.Answers[id] = a
	}
	return res, nil
}

func unit(p float64) bool { return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1 }

// deref is q as a value, so *Noul works like Noul; nil for a nil pointer.
func deref(q Question) Question {
	switch v := q.(type) {
	case *Noul:
		if v == nil {
			return nil
		}
		return *v
	case *Choice:
		if v == nil {
			return nil
		}
		return *v
	case *Score:
		if v == nil {
			return nil
		}
		return *v
	}
	return q
}

func checkAnswer(q Question, w wireAnswer) (Answer, bool) {
	q = deref(q)
	if w.Type != q.kind() {
		return Answer{}, false
	}
	a := Answer{Type: w.Type}
	switch q := q.(type) {
	case Noul:
		if w.Noul == nil || !unit(*w.Noul) {
			return a, false
		}
		a.Noul = *w.Noul
		return a, true
	case Choice:
		known := map[string]bool{}
		for _, o := range q.Options {
			known[o.Name] = true
		}
		if w.Choice == nil || !known[*w.Choice] || !checkProbs(w.Probabilities, known) || w.Confidence == nil || !unit(*w.Confidence) {
			return a, false
		}
		a.Choice, a.Probabilities, a.Confidence = *w.Choice, w.Probabilities, *w.Confidence
		return a, true
	case Score:
		known := map[string]bool{}
		for i := range q.Levels {
			known[strconv.Itoa(i)] = true
		}
		top := float64(len(q.Levels) - 1)
		if w.Score == nil || math.IsNaN(*w.Score) || *w.Score < 0 || *w.Score > top ||
			!checkProbs(w.Probabilities, known) || w.Confidence == nil || !unit(*w.Confidence) {
			return a, false
		}
		legend, ok := readLegend(w.Legend, known)
		if !ok {
			return a, false
		}
		a.Score, a.Probabilities, a.Legend, a.Confidence = *w.Score, w.Probabilities, legend, *w.Confidence
		return a, true
	}
	return a, false
}

// readLegend is a score's legend: each level, "0" up, to its description.
// A level described with an object or array (Score.Levels allows them) may
// come back as that JSON rather than a string; it is kept as its JSON text,
// so a structured level doesn't make the whole answer unreadable.
func readLegend(raw map[string]json.RawMessage, known map[string]bool) (map[string]string, bool) {
	if raw == nil {
		return nil, true
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if !known[k] {
			return nil, false
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			var c bytes.Buffer
			if json.Compact(&c, v) != nil {
				return nil, false
			}
			s = c.String()
		}
		out[k] = s
	}
	return out, true
}

// checkProbs: at least one probability, each for a known option and in
// [0, 1], adding up to about 1. TypeSafe rounds each probability to two
// decimals, so the sum may be off by up to half a hundredth per option:
// the allowance grows with the number of options.
func checkProbs(p map[string]float64, known map[string]bool) bool {
	if len(p) == 0 {
		return false
	}
	sum := 0.0
	for k, v := range p {
		if !known[k] || !unit(v) {
			return false
		}
		sum += v
	}
	slack := max(0.05, 0.005*float64(len(known))+0.01)
	return math.Abs(sum-1) < slack
}
