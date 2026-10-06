package wire

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
)

// The control stream is stream 1 of a session: newline-terminated JSON from
// the relay, one Control per line.

// Marshal encodes a control message, without the newline that ends its
// line.
func (c Control) Marshal() ([]byte, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(controlJSON{T: c.T, Gen: c.Gen, Message: c.Message, RetryAfter: c.RetryAfter, MaxStreams: c.MaxStreams, BPS: c.BPS})
	if err == nil && len(b) > MaxControlLine {
		return nil, fmt.Errorf("%w: control line too long", ErrMalformed)
	}
	return b, err
}

// ParseControl decodes and checks one control line (without its newline).
func ParseControl(b []byte) (Control, error) {
	if len(b) > MaxControlLine {
		return Control{}, fmt.Errorf("%w: control line too long", ErrMalformed)
	}
	var j controlJSON
	if err := decode(b, &j); err != nil {
		return Control{}, err
	}
	c := Control{T: j.T, Gen: j.Gen, Message: j.Message, RetryAfter: j.RetryAfter, MaxStreams: j.MaxStreams, BPS: j.BPS}
	return c, c.check()
}

// WriteControl writes c as one line of the control stream, in one Write.
func WriteControl(w io.Writer, c Control) error {
	b, err := c.Marshal()
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ControlReader reads the control stream: newline-terminated JSON, one
// message per line, each line at most MaxControlLine bytes.
type ControlReader struct{ r *bufio.Reader }

// NewControlReader reads control lines from r.
func NewControlReader(r io.Reader) *ControlReader {
	return &ControlReader{bufio.NewReaderSize(r, MaxControlLine+1)}
}

// Next returns the next message, or io.EOF when the stream ends cleanly
// between lines.
func (cr *ControlReader) Next() (Control, error) {
	line, err := cr.r.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		return Control{}, fmt.Errorf("%w: control line too long", ErrMalformed)
	case errors.Is(err, io.EOF) && len(line) == 0:
		return Control{}, io.EOF
	case errors.Is(err, io.EOF):
		return Control{}, io.ErrUnexpectedEOF
	case err != nil:
		return Control{}, err
	}
	return ParseControl(line[:len(line)-1])
}

func (c Control) check() error {
	switch {
	case !validCode(c.T):
		return fmt.Errorf("%w: control type %q", ErrMalformed, c.T)
	case c.Gen < 0, c.MaxStreams < 0, c.BPS < 0:
		return fmt.Errorf("%w: negative control value", ErrMalformed)
	case c.RetryAfter < 0 || c.RetryAfter > MaxRetryAfter:
		return fmt.Errorf("%w: retry_after", ErrMalformed)
	case !validText(c.Message):
		return fmt.Errorf("%w: message", ErrMalformed)
	}
	return nil
}
