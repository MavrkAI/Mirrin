// Package jevtest is a fake TypeSafe Jev endpoint for tests: an httptest
// server that checks requests the way the real API does and answers what a
// test scripted. It never contacts the network.
//
// A question with no scripted answer gets a neutral one (a noul of 0.5, or
// even odds with a confidence of 0), so code under test that only acts on a
// confident answer falls back, which is the safe default for a test that
// didn't say otherwise.
package jevtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/jev"
)

// key is the only key the fake accepts.
const key = "test-key"

// Model is the model name the fake answers with.
const Model = "jev-test"

// Server is a fake Jev endpoint. Its URL goes in jev.WithURL.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	script   map[string]func(WireQuestion) any
	fails    []int
	delay    time.Duration
	requests []Request
	calls    int
}

// Request is one request the fake received, decoded.
type Request struct {
	Auth      string // the Authorization header
	Model     string
	State     json.RawMessage
	Questions map[string]WireQuestion
}

// WireQuestion is a question as it was sent.
type WireQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// Options are a choice's option names in the order they were sent.
func (q WireQuestion) Options() []string {
	d := json.NewDecoder(bytes.NewReader(q.Criteria))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var out []string
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil
		}
		k, _ := t.(string)
		out = append(out, k)
		var skip json.RawMessage
		if d.Decode(&skip) != nil {
			return nil
		}
	}
	return out
}

// Levels are a score's level descriptions, lowest first.
func (q WireQuestion) Levels() []json.RawMessage {
	var out []json.RawMessage
	_ = json.Unmarshal(q.Criteria, &out)
	return out
}

// New starts a fake that closes when t ends.
func New(t testing.TB) *Server {
	s := &Server{script: map[string]func(WireQuestion) any{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Key is the key the fake accepts; any other gets a 401.
func (s *Server) Key() string { return key }

// Answer scripts the answer to the question asked under id, sent as given
// (in the wire shape for a.Type), right or wrong.
func (s *Server) Answer(id string, a jev.Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script[id] = func(WireQuestion) any { return wireAnswer(a) }
}

// Choose scripts a choice answer for id: option with probability p, the rest
// spread evenly over the other options, and confidence.
func (s *Server) Choose(id, option string, p, confidence float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script[id] = func(q WireQuestion) any {
		opts := q.Options()
		probs := map[string]float64{option: p}
		others := 0
		for _, o := range opts {
			if o != option {
				others++
			}
		}
		for _, o := range opts {
			if o != option {
				probs[o] = (1 - p) / float64(others)
			}
		}
		return map[string]any{"type": "choice", "choice": option, "probabilities": probs, "confidence": confidence}
	}
}

// RawAnswer scripts the answer for id as raw JSON, for answers no
// jev.Answer can express.
func (s *Server) RawAnswer(id, raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script[id] = func(WireQuestion) any { return json.RawMessage(raw) }
}

// Fail makes the next times requests (with the right key) get status.
func (s *Server) Fail(status, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range times {
		s.fails = append(s.fails, status)
	}
}

// Delay holds every response for d (or until the client gives up).
func (s *Server) Delay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// Requests returns every request received so far, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Calls counts the requests received so far.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in struct {
		State     json.RawMessage         `json:"state"`
		Model     string                  `json:"model"`
		Questions map[string]WireQuestion `json:"questions"`
	}
	decodeErr := json.Unmarshal(body, &in)
	s.mu.Lock()
	s.calls++
	s.requests = append(s.requests, Request{Auth: r.Header.Get("Authorization"), Model: in.Model, State: in.State, Questions: in.Questions})
	delay := s.delay
	s.mu.Unlock()

	if delay > 0 {
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	if r.Method != http.MethodPost {
		problem(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+key {
		problem(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	s.mu.Lock()
	fail := 0
	if len(s.fails) > 0 {
		fail, s.fails = s.fails[0], s.fails[1:]
	}
	s.mu.Unlock()
	if fail != 0 {
		problem(w, fail, "scripted failure")
		return
	}
	if decodeErr != nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		problem(w, http.StatusUnprocessableEntity, "body is not JSON")
		return
	}
	if why := invalid(in.State, in.Model, in.Questions); why != "" {
		problem(w, http.StatusUnprocessableEntity, why)
		return
	}
	answers := map[string]any{}
	s.mu.Lock()
	for id, q := range in.Questions {
		if f := s.script[id]; f != nil {
			answers[id] = f(q)
		} else {
			answers[id] = neutral(q)
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   Model,
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 50 + len(body)/4, "output_tokens": 10 * len(in.Questions)},
	})
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

// invalid says what is wrong with a request, as the real API refuses it
// with a 422, or "" when nothing is.
func invalid(state json.RawMessage, model string, qs map[string]WireQuestion) string {
	switch {
	case len(state) == 0 || string(state) == "null":
		return "state is required"
	case model == "":
		return "model is required"
	case len(qs) == 0:
		return "questions is required"
	}
	for id, q := range qs {
		if len(q.Instructions) == 0 || string(q.Instructions) == "null" || string(q.Instructions) == `""` {
			return id + ": instructions is required"
		}
		switch q.Type {
		case "noul":
			if len(q.Criteria) == 0 {
				continue
			}
			var c map[string]json.RawMessage
			if json.Unmarshal(q.Criteria, &c) != nil {
				return id + ": criteria must be an object"
			}
			for k := range c {
				if k != "true" && k != "false" {
					return id + ": criteria takes only true and false"
				}
			}
		case "choice":
			if n := len(q.Options()); n < 1 || n > 255 {
				return id + ": criteria needs 1 to 255 options"
			}
		case "score":
			if n := len(q.Levels()); n < 2 || n > 10 {
				return id + ": criteria needs 2 to 10 levels"
			}
		default:
			return id + ": unknown type " + strconv.Quote(q.Type)
		}
	}
	return ""
}

// neutral is the answer that tells the caller nothing.
func neutral(q WireQuestion) any {
	switch q.Type {
	case "noul":
		return map[string]any{"type": "noul", "noul": 0.5}
	case "choice":
		opts := q.Options()
		probs := map[string]float64{}
		for _, o := range opts {
			probs[o] = 1 / float64(len(opts))
		}
		return map[string]any{"type": "choice", "choice": opts[0], "probabilities": probs, "confidence": 0.0}
	default: // score
		levels := q.Levels()
		probs, legend := map[string]float64{}, map[string]string{}
		for i, l := range levels {
			k := strconv.Itoa(i)
			probs[k] = 1 / float64(len(levels))
			var s string
			if json.Unmarshal(l, &s) != nil {
				s = string(l)
			}
			legend[k] = s
		}
		return map[string]any{"type": "score", "score": float64(len(levels)-1) / 2, "legend": legend, "probabilities": probs, "confidence": 0.0}
	}
}

// wireAnswer puts a in the shape the real API sends for its type.
func wireAnswer(a jev.Answer) any {
	switch a.Type {
	case "noul":
		return map[string]any{"type": a.Type, "noul": a.Noul}
	case "choice":
		return map[string]any{"type": a.Type, "choice": a.Choice, "probabilities": a.Probabilities, "confidence": a.Confidence}
	case "score":
		return map[string]any{"type": a.Type, "score": a.Score, "legend": a.Legend, "probabilities": a.Probabilities, "confidence": a.Confidence}
	}
	return map[string]any{"type": a.Type}
}
