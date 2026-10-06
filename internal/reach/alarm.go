package reach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/health"
)

// The alarm playbook (docs/cloud-design.md §6.5). It runs on its own when
// certwatch finds a certificate this machine didn't request, when CAA lets
// another ACME account issue, or when a relay says another machine took
// the name:
//
//  1. Remote approvals are paused (423) until the owner clears the alarm
//     on this computer or in their own chat.
//  2. Devices that used the public name since the rogue notBefore get new
//     credentials: their keys stop working and they see a re-pair page.
//  3. Passkeys enrolled in that window are revoked.
//  4. Every device's next response carries Clear-Site-Data, and the
//     service worker version changes.
//  5. A security push, a presence-screen banner, a message in the owner's
//     chat, and a health Fail.
//
// Browsers can't pin keys, so while an impostor with a valid certificate
// lasts it can fool a browser that reaches it. Detection, this playbook
// and BYOD (byod.go) are the answers.

// PasskeyRevoker revokes WebAuthn credentials enrolled at or after a time.
// The default, StorePasskeys, drops them from devices.json on every device,
// rotated or not; WP-06 (passkey step-up) may supply its own. Passkeys on
// devices the playbook rotates go with the device either way
// (devices.Store.Revoke drops them).
type PasskeyRevoker interface {
	RevokePasskeysSince(ctx context.Context, since time.Time) (int, error)
}

// StorePasskeys revokes the passkeys the devices store holds.
type StorePasskeys func() *devices.Store

func (f StorePasskeys) RevokePasskeysSince(_ context.Context, since time.Time) (int, error) {
	if f == nil {
		return 0, nil
	}
	s := f()
	if s == nil {
		return 0, nil
	}
	return s.DropPasskeysSince(since)
}

// clearSiteFor is how long after the alarm a stale credential still gets
// Clear-Site-Data.
const clearSiteFor = 30 * 24 * time.Hour

// Alarm is the persistent alarm state (data/reach/alarm.json). It
// implements api.Alarm.
type Alarm struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
	st   AlarmState
}

// AlarmState is what alarm.json holds.
type AlarmState struct {
	Active    bool      `json:"active"`
	Since     time.Time `json:"since,omitzero"`
	Kind      string    `json:"kind,omitempty"`
	Host      string    `json:"host,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	NotBefore time.Time `json:"not_before,omitzero"`
	// Handled are finding keys already acted on, so a finding seen again
	// after a restart doesn't run the playbook twice.
	Handled []string `json:"handled,omitempty"`
	// Epoch goes into the service worker version.
	Epoch int `json:"epoch"`
	// ClearSite lists devices still owed Clear-Site-Data.
	ClearSite []string  `json:"clear_site,omitempty"`
	Rotated   []string  `json:"rotated,omitempty"`
	ClearedAt time.Time `json:"cleared_at,omitzero"`
	ClearedBy string    `json:"cleared_by,omitempty"`
}

// AlarmPath is where a twin keeps its alarm state.
func AlarmPath(dataDir string) string { return filepath.Join(dataDir, "reach", "alarm.json") }

// OpenAlarm loads the alarm, or starts a quiet one. A damaged file opens as
// an active alarm: it is safer to pause approvals than to forget one.
func OpenAlarm(dataDir string) (*Alarm, error) {
	a := &Alarm{path: AlarmPath(dataDir), now: time.Now}
	b, err := os.ReadFile(a.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return a, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &a.st); err != nil {
		a.st = AlarmState{Active: true, Since: a.now(), Kind: "damaged", Detail: "The alarm record was damaged, so approvals from other devices stay paused until you clear it.", Epoch: 1}
	}
	return a, nil
}

func (a *Alarm) save() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// State is a copy of the alarm.
func (a *Alarm) State() AlarmState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.st
	st.Handled, st.ClearSite, st.Rotated = slices.Clone(st.Handled), slices.Clone(st.ClearSite), slices.Clone(st.Rotated)
	return st
}

// Active reports whether remote approvals are paused.
func (a *Alarm) Active() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.Active
}

// Epoch changes each time the alarm fires.
func (a *Alarm) Epoch() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.Epoch == 0 {
		return ""
	}
	return "alarm-" + strconv.Itoa(a.st.Epoch)
}

// ClearSite reports, once per device, whether its next response carries
// Clear-Site-Data. "" (a credential that no longer works) gets it while
// the alarm is recent.
func (a *Alarm) ClearSite(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id == "" {
		return a.st.Epoch > 0 && (a.st.Active || a.now().Before(a.st.Since.Add(clearSiteFor)))
	}
	i := slices.Index(a.st.ClearSite, id)
	if i < 0 {
		return false
	}
	a.st.ClearSite = slices.Delete(a.st.ClearSite, i, i+1)
	_ = a.save()
	return true
}

// Acknowledged reports whether a finding (by its Key) raised an alarm the
// owner has since cleared: the watcher stops failing health over it.
func (a *Alarm) Acknowledged(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.st.Active && slices.Contains(a.st.Handled, key)
}

// Clear ends the pause. by says who: "loopback" or a chat.
func (a *Alarm) Clear(by string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.st.Active {
		return nil
	}
	a.st.Active, a.st.ClearedAt, a.st.ClearedBy = false, a.now(), by
	return a.save()
}

// raise records a finding. It reports false if that finding was handled
// before.
func (a *Alarm) raise(f certwatch.Finding, window time.Time) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := f.Key()
	if slices.Contains(a.st.Handled, key) {
		return false, nil
	}
	a.st.Handled = append(a.st.Handled, key)
	if !a.st.Active {
		a.st.Since = a.now()
	}
	a.st.Active, a.st.Kind, a.st.Host, a.st.Detail, a.st.NotBefore = true, string(f.Kind), f.Host, f.Detail, window
	a.st.Epoch++
	return true, a.save()
}

func (a *Alarm) owe(clear, rotated []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range clear {
		if !slices.Contains(a.st.ClearSite, id) {
			a.st.ClearSite = append(a.st.ClearSite, id)
		}
	}
	a.st.Rotated = append(a.st.Rotated, rotated...)
	return a.save()
}

// Health is the alarm's line on the health page.
func (a *Alarm) Health(context.Context) (health.State, string, string) {
	st := a.State()
	if st.Active {
		return health.Fail, "certificate alarm: " + st.Detail, "Approve on this computer for now. When you've checked, run `mirrin reach alarm clear` (or send /alarm clear in your own chat)"
	}
	return health.OK, "no certificate alarm", ""
}

// Playbook runs the alarm for a critical finding. It is safe to call again
// with the same finding: only the first call acts.
func Playbook(ctx context.Context, f certwatch.Finding, d Deps) error {
	if f.Severity != certwatch.Critical {
		return nil
	}
	if d.Alarm == nil {
		return errors.New("reach: the playbook needs the alarm")
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	window := f.NotBefore
	if window.IsZero() || window.After(now) {
		window = now
		if !f.At.IsZero() && f.At.Before(now) {
			window = f.At
		}
	}
	fresh, err := d.Alarm.raise(f, window)
	if err != nil || !fresh {
		return err
	}
	text := alarmText(f)
	// Tell the owner first: the push goes to devices before any of them is
	// cut off.
	if d.Push != nil {
		d.Push("alarm-"+f.Key(), text)
	}
	var errs []error
	var rotated, clear []string
	if d.Devices != nil {
		if store := d.Devices(); store != nil {
			for _, dev := range store.List() {
				if dev.Revoked() || dev.Local() {
					continue
				}
				last := dev.LastSeen
				if last.IsZero() || dev.Created.After(last) {
					last = dev.Created
				}
				// Other devices only reach this machine through a remote
				// listener; a terminal on this computer paired and used on
				// loopback never touched the public name.
				remote := dev.Via != "" && dev.Via != "loopback" || dev.LastIP != "" && !loopback(dev.LastIP)
				if !last.Before(window) && remote {
					if _, err := store.Revoke(dev.ID); err != nil {
						errs = append(errs, err)
						continue
					}
					rotated = append(rotated, dev.ID)
					continue
				}
				clear = append(clear, dev.ID)
			}
		}
	}
	pk := d.Passkeys
	if pk == nil {
		pk = StorePasskeys(d.Devices)
	}
	if _, err := pk.RevokePasskeysSince(ctx, window); err != nil {
		errs = append(errs, err)
	}
	if err := d.Alarm.owe(clear, rotated); err != nil {
		errs = append(errs, err)
	}
	full := text
	if len(rotated) > 0 {
		full += fmt.Sprintf(" I signed out %d device(s) that used your address since then; pair them again with `mirrin pair`.", len(rotated))
	}
	if d.Audit != nil {
		d.Audit("reach.alarm", fmt.Sprintf("%s %s: %s (window from %s; rotated %v)", f.Kind, f.Host, f.Detail, window.UTC().Format(time.RFC3339), rotated))
	}
	if d.Banner != nil {
		d.Banner(full)
	}
	if d.Notify != nil {
		if err := d.Notify(ctx, full); err != nil {
			errs = append(errs, err)
		}
	}
	if d.Health != nil {
		go d.Health.Run(context.WithoutCancel(ctx))
	}
	return errors.Join(errs...)
}

func alarmText(f certwatch.Finding) string {
	what := f.Detail
	switch f.Kind {
	case certwatch.Superseded:
		what = "Another computer took over " + f.Host + " at the relay."
	}
	return what + " I've paused approvals from other devices until you check. Approve on this computer for now, and when you're sure, run `mirrin reach alarm clear` on this computer or send /alarm clear in your own chat."
}

func loopback(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && p.IsLoopback()
}
