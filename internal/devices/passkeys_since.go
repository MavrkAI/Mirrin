package devices

import "time"

// DropPasskeysSince removes, from every device, the passkeys enrolled at or
// after since, and reports how many went. The certificate alarm's playbook
// (internal/reach/alarm.go) uses it: whoever held a rogue certificate for
// the twin's address from since on may have enrolled one of their own.
// Devices keep their keys and older passkeys.
func (s *Store) DropPasskeysSince(since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type undo struct {
		d  *Device
		pk []Passkey
	}
	var undos []undo
	n := 0
	for _, d := range s.devs {
		var keep []Passkey
		for _, p := range d.Passkeys {
			if p.Created.Before(since) {
				keep = append(keep, p)
			}
		}
		if len(keep) == len(d.Passkeys) {
			continue
		}
		undos = append(undos, undo{d, d.Passkeys})
		n += len(d.Passkeys) - len(keep)
		d.Passkeys = keep
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.save(); err != nil {
		for _, u := range undos {
			u.d.Passkeys = u.pk
		}
		return 0, err
	}
	return n, nil
}
