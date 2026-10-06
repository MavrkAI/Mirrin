package channels

import (
	"errors"
	"strings"
)

type partialError struct {
	err  error
	rest string
}

func (e partialError) Error() string { return e.err.Error() }
func (e partialError) Unwrap() error { return e.err }

// SendParts sends text in pieces of at most n bytes (see SplitText), one call
// to send each. If a piece fails after others went out, the error remembers
// what is left, so the rest can be delivered elsewhere without repeating what
// already arrived (see Unsent).
func SendParts(text string, n int, send func(part string) error) error {
	parts := SplitText(text, n)
	for i, part := range parts {
		if err := send(part); err != nil {
			if i == 0 {
				return err
			}
			return partialError{err: err, rest: strings.Join(parts[i:], "\n")}
		}
	}
	return nil
}

// Unsent is what a failed Send of text didn't deliver: all of it, unless the
// error says some parts went out.
func Unsent(err error, text string) string {
	var p partialError
	if errors.As(err, &p) {
		return p.rest
	}
	return text
}
