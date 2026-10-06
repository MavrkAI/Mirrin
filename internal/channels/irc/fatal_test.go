package irc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

// Settings only the owner can fix stop the channel instead of retrying
// forever and showing as "reconnecting".
func TestProblemsOnlyTheOwnerCanFixStopTheChannel(t *testing.T) {
	h := func(context.Context, channels.Inbound) {}
	if err := New("", false, "mavrk", "", "akshay", false, nil, nil).Start(context.Background(), h); !channels.IsFatal(err) {
		t.Fatalf("missing server: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte(":irc.example 464 mavrk :Password incorrect\r\n"))
		time.Sleep(time.Second)
	}()
	done := make(chan error, 1)
	go func() {
		done <- New(ln.Addr().String(), false, "mavrk", "wrong", "akshay", false, nil, nil).Start(context.Background(), h)
	}()
	select {
	case err := <-done:
		if !channels.IsFatal(err) || !strings.Contains(err.Error(), "password") {
			t.Fatalf("rejected password: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a rejected password was retried")
	}
}
