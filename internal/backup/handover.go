package backup

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
)

// A handover marker tells the machine a twin moved away from that it should
// stand by: pause, stop its channels, stop backing up, and say where the
// twin went. Restoring a snapshot writes one to the snapshot's own target.
//
// The marker is encrypted with age to the old machine's standby key (named
// in the snapshot's manifest) and to the words, so nobody else, not even
// whoever stores it, can read which machine the twin moved to. It is signed
// with the recovery key, which the old machine checks against its
// backup.recovery_pub, so a marker nobody with the words wrote is ignored.

// Handover says where a twin went.
type Handover struct {
	// Name is the marker's object name.
	Name      string    `json:"name"`
	HostLabel string    `json:"host_label"`
	At        time.Time `json:"at"`
	// Seq is the number of the snapshot that was restored.
	Seq int64 `json:"seq"`
}

// handoverPayload is what a marker says; it is signed as it is stored.
type handoverPayload struct {
	Format    int       `json:"format"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
	HostLabel string    `json:"host_label"`
	Seq       int64     `json:"seq"`
	// To is the standby recipient of the machine it is meant for.
	To string `json:"to"`
}

type handoverFile struct {
	Payload json.RawMessage `json:"payload"`
	Sig     string          `json:"sig"`
}

const handoverContext = "antbot-handover-v1\n"

// maxMarker bounds a marker a machine reads.
const maxMarker = 64 << 10

// writeHandover leaves a marker for the machine whose standby recipient is
// to, saying the twin moved to host.
func writeHandover(ctx context.Context, t Target, p Phrase, to, host string, seq int64, now time.Time) (string, error) {
	oldMachine, err := age.ParseHybridRecipient(to)
	if err != nil {
		return "", fmt.Errorf("the snapshot names no machine to hand over from (%v)", err)
	}
	payload, err := json.Marshal(handoverPayload{Format: 1, Kind: "handover", At: now.UTC().Truncate(time.Second), HostLabel: host, Seq: seq, To: to})
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(p.RecoveryKey(), append([]byte(handoverContext), payload...))
	body, err := json.Marshal(handoverFile{Payload: payload, Sig: base64.RawURLEncoding.EncodeToString(sig)})
	if err != nil {
		return "", err
	}
	recipients := []age.Recipient{oldMachine}
	if mine, err := age.ParseHybridRecipient(p.Recipient()); err == nil {
		recipients = append(recipients, mine)
	}
	var buf strings.Builder
	w, err := age.Encrypt(&buf, recipients...)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(body); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	name := newName("handover", now)
	if err := t.Put(ctx, name, strings.NewReader(buf.String()), int64(buf.Len())); err != nil {
		return "", err
	}
	return name, nil
}

// CheckHandover looks for a marker meant for this machine (the one whose
// standby key is standby) that the owner hasn't dismissed and that is no
// older than notBefore (when a restore last made this machine the twin).
// It returns nil when there is none.
func CheckHandover(ctx context.Context, t Target, standby *age.HybridIdentity, recoveryPub string, dismissed []string, notBefore time.Time) (*Handover, error) {
	if standby == nil {
		return nil, nil // this machine never took a snapshot, so no snapshot names it
	}
	pub, err := ParseRecoveryPub(recoveryPub)
	if err != nil {
		return nil, err
	}
	objs, err := t.List(ctx)
	if err != nil {
		return nil, err
	}
	me := standby.Recipient().String()
	for i := len(objs) - 1; i >= 0; i-- {
		o := objs[i]
		if kind, _, ok := parseName(o.Name); !ok || kind != "handover" || contains(dismissed, o.Name) || o.Size > maxMarker*2 {
			continue
		}
		h, err := readHandover(ctx, t, o.Name, standby, pub, me)
		if err != nil || h == nil {
			continue // for another machine, or not written with the words
		}
		if h.At.Before(notBefore) {
			continue // the twin moved back here since
		}
		return h, nil
	}
	return nil, nil
}

func readHandover(ctx context.Context, t Target, name string, standby age.Identity, pub ed25519.PublicKey, me string) (*Handover, error) {
	rc, err := t.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	plain, err := age.Decrypt(io.LimitReader(rc, maxMarker*2), standby)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(plain, maxMarker))
	if err != nil {
		return nil, err
	}
	var f handoverFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(f.Sig)
	if err != nil || !ed25519.Verify(pub, append([]byte(handoverContext), f.Payload...), sig) {
		return nil, errors.New("not signed with this twin's recovery key")
	}
	var p handoverPayload
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		return nil, err
	}
	if p.Kind != "handover" || p.To != me {
		return nil, errors.New("not meant for this machine")
	}
	return &Handover{Name: name, HostLabel: p.HostLabel, At: p.At, Seq: p.Seq}, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// StandBy records a handover: from now on this machine stays paused and
// takes no backups, until `mirrin backup resume`.
func StandBy(dataDir string, h Handover) error {
	return UpdateState(dataDir, func(s *State) { s.Standby = &h })
}

// Resume makes this machine the twin again: it forgets the handover and
// ignores that marker from now on.
func Resume(dataDir string) (*Handover, error) {
	var was *Handover
	err := UpdateState(dataDir, func(s *State) {
		was = s.Standby
		if was != nil && !contains(s.Dismissed, was.Name) {
			s.Dismissed = append(s.Dismissed, was.Name)
		}
		s.Standby = nil
		if was != nil {
			// The twin wrote elsewhere meanwhile, and the other machine's
			// retention may have pruned this one's last (gone.go).
			clearLast(s)
		}
	})
	return was, err
}

// StandbyMessage is how a standing-by machine describes itself.
func StandbyMessage(h *Handover) string {
	if h == nil {
		return ""
	}
	host := strings.TrimSpace(h.HostLabel)
	if host == "" {
		host = "another machine"
	}
	return "Standing by: moved to " + host
}
