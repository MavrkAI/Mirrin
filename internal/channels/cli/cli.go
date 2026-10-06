// Package cli is the terminal channel: type to Mirrin, read replies.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Channel reads lines from stdin.
type Channel struct {
	in     io.Reader
	out    io.Writer
	prompt string
}

// New builds a terminal channel.
func New(name string) *Channel {
	return &Channel{in: os.Stdin, out: os.Stdout, prompt: name}
}

func (c *Channel) Name() string        { return "cli" }
func (c *Channel) OwnerChatID() string { return "terminal" }

// Start reads stdin until EOF or ctx ends.
func (c *Channel) Start(ctx context.Context, handler channels.Handler) error {
	fmt.Fprintf(c.out, "%s online. Type a message, or /quit.\n> ", c.prompt)
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(c.in)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			line = strings.TrimSpace(line)
			if line == "" {
				fmt.Fprint(c.out, "> ")
				continue
			}
			if line == "/quit" || line == "/exit" {
				return io.EOF
			}
			handler(ctx, channels.Inbound{Channel: "cli", ChatID: "terminal", Sender: "owner", Text: line, IsOwner: true})
			fmt.Fprint(c.out, "> ")
		}
	}
}

// Send prints a reply.
func (c *Channel) Send(_ context.Context, _ string, text string) error {
	fmt.Fprintf(c.out, "\n%s: %s\n> ", c.prompt, text)
	return nil
}

type cliStream struct {
	c     *Channel
	wrote bool
}

func (s *cliStream) Write(delta string) {
	if !s.wrote {
		fmt.Fprintf(s.c.out, "\n%s: ", s.c.prompt)
		s.wrote = true
	}
	fmt.Fprint(s.c.out, delta)
}

// Note prints a status line while the reply is in progress.
func (s *cliStream) Note(text string) {
	if s.wrote {
		fmt.Fprint(s.c.out, "\n")
	}
	fmt.Fprintf(s.c.out, "  … %s\n", text)
	s.wrote = false
}

func (s *cliStream) Close() {
	if s.wrote {
		fmt.Fprint(s.c.out, "\n> ")
	}
}

// OpenStream prints the reply as it arrives.
func (c *Channel) OpenStream(_ context.Context, _ string) channels.Stream { return &cliStream{c: c} }
