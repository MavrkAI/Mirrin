//go:build !nowhatsapp

package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func noHandler(context.Context, channels.Inbound) {}

// The Channels page shows the connection as it really is: connecting until
// WhatsApp accepts the link, connected, and reconnecting after a drop.
func TestStatusFollowsTheConnection(t *testing.T) {
	c := New(t.TempDir(), "+61400000001", false, nil)
	stop := make(chan error, 1)
	ev := func(e any) { c.onEvent(context.Background(), e, noHandler, stop) }
	if st := c.Status(); st.State != channels.Connecting {
		t.Fatalf("before connecting: %+v", st)
	}
	ev(&events.Connected{})
	if st := c.Status(); st.State != channels.Connected || st.Err != "" {
		t.Fatalf("connected: %+v", st)
	}
	ev(&events.Disconnected{})
	if st := c.Status(); st.State != channels.Connecting || !strings.Contains(st.Err, "dropped") {
		t.Fatalf("dropped: %+v", st)
	}
	ev(&events.Connected{})
	ev(&events.KeepAliveTimeout{ErrorCount: 1})
	if c.Status().State != channels.Connected {
		t.Fatal("one missed ping is not a lost connection")
	}
	ev(&events.KeepAliveTimeout{ErrorCount: 3})
	if st := c.Status(); st.State != channels.Connecting || !strings.Contains(st.Err, "online") {
		t.Fatalf("pings failing: %+v", st)
	}
	ev(&events.KeepAliveRestored{})
	if c.Status().State != channels.Connected {
		t.Fatal("pings back: connected again")
	}
	select {
	case err := <-stop:
		t.Fatalf("an ordinary drop stopped the channel: %v", err)
	default:
	}
}

// When the phone unlinks this computer the channel stops for good (the
// daemon then tells the owner elsewhere) and says how to pair again; a
// stream taken over elsewhere or an outdated client says what to do too.
func TestLoggedOutStopsAndSaysHowToPairAgain(t *testing.T) {
	for _, tt := range []struct {
		event any
		fatal bool
		want  string
	}{
		{&events.LoggedOut{Reason: events.ConnectFailureLoggedOut}, true, "Pair by scanning a QR code"},
		{&events.StreamReplaced{}, true, "another computer"},
		{&events.ClientOutdated{}, true, "update Mirrin"},
		{&events.TemporaryBan{Code: events.TempBanBlockedByUsers, Expire: 90 * time.Minute}, true, "for about 2 hours (too many people blocked you)"},
		{&events.ConnectFailure{Reason: events.ConnectFailureGeneric}, false, "refused"},
	} {
		c := New(t.TempDir(), "+61400000001", false, nil)
		stop := make(chan error, 1)
		c.onEvent(context.Background(), &events.Connected{}, noHandler, stop)
		c.onEvent(context.Background(), tt.event, noHandler, stop)
		c.onEvent(context.Background(), tt.event, noHandler, stop) // a repeat must not block the event loop
		var err error
		select {
		case err = <-stop:
		default:
			t.Fatalf("%T: the channel kept running", tt.event)
		}
		if got := c.stopped(err); got != err || channels.IsFatal(err) != tt.fatal || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%T: err %v (fatal %v)", tt.event, err, channels.IsFatal(err))
		}
		st := c.Status()
		if want := map[bool]channels.State{true: channels.Failed, false: channels.Connecting}[tt.fatal]; st.State != want || !strings.Contains(st.Err, tt.want) {
			t.Errorf("%T: status %+v", tt.event, st)
		}
	}
}

// Without a linked device there is nothing to reconnect: the channel stops
// and the page says how to link it.
func TestUnpairedStartSaysHowToLink(t *testing.T) {
	c := New(t.TempDir(), "+61400000001", false, nil)
	defer c.Close()
	err := c.Start(context.Background(), noHandler)
	if !channels.IsFatal(err) || !strings.Contains(err.Error(), "Pair") {
		t.Fatalf("err %v", err)
	}
	if st := c.Status(); st.State != channels.Failed || !strings.Contains(st.Err, "Pair") {
		t.Fatalf("status %+v", st)
	}
}

// The device store opens at exactly <data>/whatsapp.db however the data
// folder is named: SQLite would read '#' as a fragment, '%' as an escape and
// the driver would split at '?', opening (and creating) some other file.
func TestDataFolderNameCanHoldAnyCharacter(t *testing.T) {
	parent := t.TempDir()
	name := "twin #1 ?50% off"
	if runtime.GOOS == "windows" {
		name = "twin #1 50% off" // Windows allows no '?' in a name
	}
	dir := filepath.Join(parent, name)
	c := New(dir, "+61400000001", false, nil)
	_ = c.Start(context.Background(), noHandler) // unpaired: opens the store, then stops
	c.Close()
	if _, err := os.Stat(filepath.Join(dir, "whatsapp.db")); err != nil {
		t.Fatalf("store not where it belongs: %v", err)
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 || entries[0].Name() != name {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("stray files next to the data folder: %q", names)
	}
	if Paired(dir) {
		t.Fatal("nothing is linked yet")
	}
}

// The indicator is a courtesy: before connecting it is refused, not a crash.
func TestTypingNeedsAConnection(t *testing.T) {
	c := New(t.TempDir(), "+61400000001", false, nil)
	var typer channels.Typer = c
	var ender channels.TypingEnder = c
	if _, err := typer.Typing(context.Background(), c.OwnerChatID()); err == nil {
		t.Fatal("typing while not connected")
	}
	if err := ender.EndTyping(context.Background(), c.OwnerChatID()); err == nil {
		t.Fatal("clearing typing while not connected")
	}
}

// A ban's length reads the way a person would say it.
func TestBanLengthsArePlain(t *testing.T) {
	for d, want := range map[time.Duration]string{
		40 * time.Second:                "about 1 minute",
		20 * time.Minute:                "about 20 minutes",
		59*time.Minute + 50*time.Second: "about 1 hour",
		90 * time.Minute:                "about 2 hours",
		26 * time.Hour:                  "about 26 hours",
		50 * time.Hour:                  "about 2 days",
	} {
		if got := about(d); got != want {
			t.Errorf("about(%v) = %q, want %q", d, got, want)
		}
	}
}

// Linked to the owner's own number, the owner's chat is the account's own
// "message yourself" chat, where nobody would see "typing…": nothing is
// sent there. Anyone else's chat still gets it.
func TestNoTypingInTheAccountsOwnChat(t *testing.T) {
	c := New(t.TempDir(), "+61400000001", false, nil)
	self := types.NewJID("61400000001", types.DefaultUserServer)
	c.setClient(whatsmeow.NewClient(&store.Device{ID: &self}, nil))
	if again, err := c.Typing(context.Background(), c.OwnerChatID()); err != nil || again != 0 {
		t.Fatalf("typing in the account's own chat: %v %v", again, err)
	}
	if err := c.EndTyping(context.Background(), c.OwnerChatID()); err != nil {
		t.Fatalf("clearing it there: %v", err)
	}
	friend := types.NewJID("61400000002", types.DefaultUserServer).String()
	if _, err := c.Typing(context.Background(), friend); err == nil {
		t.Fatal("a friend's chat is not skipped (and needs a connection)")
	}
}

// The daemon finds the connection state and the indicator by interface.
var (
	_ channels.Reporter    = (*Channel)(nil)
	_ channels.TypingEnder = (*Channel)(nil)
	_ channels.Typer       = (*Channel)(nil)
)
