package push

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	mu   sync.Mutex
	path string
	subs map[string]Subscription
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, subs: map[string]Subscription{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &s.subs); err != nil {
		return nil, err
	}
	if s.subs == nil {
		return nil, errors.New("invalid push store")
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(s.subs)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".push-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), s.path)
}
func (s *Store) Put(sub Subscription) error {
	if sub.DeviceID == "" {
		return errors.New("push needs a device")
	}
	if err := sub.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.subs[sub.Endpoint]
	if !exists && len(s.subs) >= 1024 {
		return errors.New("too many push subscriptions")
	}
	s.subs[sub.Endpoint] = sub
	if err := s.save(); err != nil {
		if exists {
			s.subs[sub.Endpoint] = old
		} else {
			delete(s.subs, sub.Endpoint)
		}
		return err
	}
	return nil
}
func (s *Store) Delete(device, endpoint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := map[string]Subscription{}
	for k, v := range s.subs {
		if v.DeviceID == device && (endpoint == "" || endpoint == k) {
			old[k] = v
			delete(s.subs, k)
		}
	}
	if err := s.save(); err != nil {
		for k, v := range old {
			s.subs[k] = v
		}
		return err
	}
	return nil
}
func (s *Store) List() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Subscription, 0, len(s.subs))
	for _, v := range s.subs {
		out = append(out, v)
	}
	return out
}
func (s *Store) contains(sub Subscription) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs[sub.Endpoint] == sub
}
