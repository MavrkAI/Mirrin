// Package jev is a small client for TypeSafe's Jev, a "System One" model
// that answers typed questions about a state with probabilities instead of
// text: a yes probability (noul), one option of a set (choice) or a level
// on a scale (score).
//
// The twin never needs it. Callers ask only after their own exact rule
// didn't settle something, and treat every error, including a malformed or
// unsure answer, as "do what you would have done without it". So the client
// is strict: one short deadline for the whole call, one retry when TypeSafe
// is busy, and every answer checked before anyone reads it.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const (
	// DefaultURL is TypeSafe's evaluation endpoint.
	DefaultURL = "https://api.typesafe.ai/v1/systemone"
	// DefaultModel is TypeSafe's alias for its newest Jev.
	DefaultModel = "jev-latest"
	// DefaultTimeout bounds the whole call, the one retry included.
	DefaultTimeout = 2 * time.Second
)

// maxBody is the most of a response that is read. An answer to a few
// questions is well under a kilobyte.
const maxBody = 64 << 10

// retryWait is the pause before the one retry when TypeSafe gives no
// Retry-After; up to retryJitter is added.
const (
	retryWait   = 250 * time.Millisecond
	retryJitter = 100 * time.Millisecond
)

var (
	// ErrNoKey is returned, with no request made, by a client without a key.
	ErrNoKey = errors.New("jev: no key")
	// ErrBadAnswer is a response that couldn't be read, or that doesn't
	// answer what was asked in the shape it was asked.
	ErrBadAnswer = errors.New("jev: malformed answer")
	// ErrInvalid is a question or state refused before anything was sent.
	ErrInvalid = errors.New("jev: invalid question")
)

// StatusError is a refusal from TypeSafe. It carries only the status code:
// never the key, the state or the response body.
type StatusError struct {
	Code      int
	Retryable bool // 429 (rate limit) or 529 (overloaded)
}

func (e *StatusError) Error() string { return "jev: status " + strconv.Itoa(e.Code) }

// Client asks Jev questions. It is safe for concurrent use.
type Client struct {
	key, url, model, ua string
	hc                  *http.Client
	timeout             time.Duration
	sleep               func(context.Context, time.Duration) error
	jitter              func() time.Duration
}

// Option changes a Client made by New.
type Option func(*Client)

// WithURL sends requests to u instead of DefaultURL (tests point it at a
// jevtest server).
func WithURL(u string) Option { return func(c *Client) { c.url = u } }

// WithModel asks model instead of DefaultModel.
func WithModel(m string) Option { return func(c *Client) { c.model = m } }

// WithTimeout bounds the whole call, the retry included. The caller's own
// deadline wins when it is sooner.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithHTTPClient sends through hc. The default is a plain http.Client on
// http.DefaultTransport, so whatever watches that transport sees every call.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithUserAgent names the caller, such as "mirrin/1.2.0".
func WithUserAgent(ua string) Option { return func(c *Client) { c.ua = ua } }

// New returns a client that asks with key. An empty key makes a client
// whose every Ask returns ErrNoKey.
func New(key string, opts ...Option) *Client {
	c := &Client{key: key, url: DefaultURL, model: DefaultModel, ua: "mirrin", timeout: DefaultTimeout,
		sleep: sleepCtx, jitter: func() time.Duration { return rand.N(retryJitter) }}
	for _, o := range opts {
		o(c)
	}
	if c.hc == nil {
		c.hc = &http.Client{}
	}
	if c.url == "" {
		c.url = DefaultURL
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	return c
}

// Usage is what a request cost, in TypeSafe's tokens.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Result is the answers to one request, by the ids they were asked under.
// Every asked id has an answer of the asked type: Ask checks that.
type Result struct {
	Model   string
	Answers map[string]Answer
	Usage   Usage
}

// Answer is one judgment.
type Answer struct {
	Type string // "noul", "choice" or "score"
	// Noul is the probability the answer is yes (noul).
	Noul float64
	// Choice is the most likely option (choice).
	Choice string
	// Probabilities maps each option (choice) or level, "0" up (score), to
	// its probability.
	Probabilities map[string]float64
	// Score is the probability-weighted level, from 0 (score).
	Score float64
	// Legend maps each level, "0" up, to its description (score).
	Legend map[string]string
	// Confidence is how sure Jev is, from 0 to 1 (choice and score).
	Confidence float64
}

// P is the probability of option (or score level), 0 when it has none.
func (a Answer) P(option string) float64 { return a.Probabilities[option] }

type wireRequest struct {
	State     any                     `json:"state"`
	Model     string                  `json:"model"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   Usage                      `json:"usage"`
}

// Ask sends state with every question in qs in one request and returns the
// answers by id. Questions are checked first, and a bad one is refused with
// ErrInvalid before anything is sent. TypeSafe's 429 or 529 is retried once,
// if the wait fits before the deadline; any other refusal is a *StatusError,
// and an answer that doesn't fit its question is ErrBadAnswer.
func (c *Client) Ask(ctx context.Context, state any, qs map[string]Question) (*Result, error) {
	if c == nil || c.key == "" {
		return nil, ErrNoKey
	}
	wq, err := wireQuestions(qs)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, fmt.Errorf("%w: no state", ErrInvalid)
	}
	body, err := json.Marshal(wireRequest{State: state, Model: c.model, Questions: wq})
	if err != nil {
		return nil, fmt.Errorf("%w: the state or a question can't be sent as JSON", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		raw, wait, err := c.post(ctx, body)
		if err == nil {
			return decode(raw, qs)
		}
		var se *StatusError
		if attempt > 0 || !errors.As(err, &se) || !se.Retryable {
			return nil, err
		}
		if wait <= 0 {
			wait = retryWait + c.jitter()
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= wait {
			return nil, err // the retry couldn't finish in time
		}
		if c.sleep(ctx, wait) != nil {
			return nil, fmt.Errorf("jev: %w", ctx.Err())
		}
	}
}

// post sends one request. It returns the body of a 200, or the refusal and
// how long TypeSafe asked to wait before trying again.
func (c *Client) post(ctx context.Context, body []byte) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("jev: bad URL")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, fmt.Errorf("jev: %w", ctx.Err())
		}
		return nil, 0, errors.New("jev: couldn't reach TypeSafe")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if resp.StatusCode != http.StatusOK {
		code := resp.StatusCode
		return nil, retryAfter(resp.Header.Get("Retry-After")), &StatusError{Code: code, Retryable: code == http.StatusTooManyRequests || code == 529}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, fmt.Errorf("jev: %w", ctx.Err())
		}
		return nil, 0, ErrBadAnswer
	}
	if len(raw) > maxBody {
		return nil, 0, ErrBadAnswer
	}
	return raw, 0, nil
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		if s < 0 {
			return 0
		}
		return time.Duration(min(s, 3600)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
