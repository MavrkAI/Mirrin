package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
)

//go:embed channels.html
var channelsHTML []byte

// Link is a button the page shows for a connector (e.g. a Discord invite).
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// ConnectorState is a connector plus its live status.
type ConnectorState struct {
	config.Connector
	Running   bool   `json:"running"`
	Error     string `json:"error,omitempty"`
	OwnerChat string `json:"owner_chat,omitempty"`
	Notice    string `json:"notice,omitempty"`
	Links     []Link `json:"links,omitempty"`
	// WhatsApp only: whether a device is stored, and any pairing in progress.
	Paired  bool `json:"paired,omitempty"`
	Pairing any  `json:"pairing,omitempty"`
	// State is "reconnecting" while a channel that lost its connection tries
	// again (the page shows it amber, not as a failure); Since is when its
	// connection last changed.
	State string    `json:"state,omitempty"`
	Since time.Time `json:"since,omitzero"`
}

// ChannelsBackend is what the Channels page needs.
type ChannelsBackend interface {
	ConnectorStates(ctx context.Context) []ConnectorState
	ConnectChannel(ctx context.Context, name string, fields map[string]string) error
	DisconnectChannel(ctx context.Context, name string) error
	TestChannel(ctx context.Context, name string) error
	// ConnectFields saves fields without starting the channel.
	ConnectFields(ctx context.Context, name string, fields map[string]string) error
	PairWhatsApp(ctx context.Context, byPhone bool) error
	WhatsAppPairing() *whatsapp.Snapshot
	UnpairWhatsApp(ctx context.Context) error
}

// WithChannels enables the Channels page.
func (s *Server) WithChannels(b ChannelsBackend) *Server { s.chans = b; return s }

// ChannelsURL is the connectors page link.
func (s *Server) ChannelsURL() string { return s.localBase() + "/channels?token=" + s.masterKey() }

// channelRoutes serves the Channels page: settings, so admin on this computer.
func (s *Server) channelRoutes(mux *http.ServeMux, a authz) {
	mux.HandleFunc("GET /channels", s.page(a, devices.Admin, channelsHTML, nil))
	mux.HandleFunc("GET /channels/list", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		st := s.chans.ConnectorStates(r.Context())
		if st == nil {
			st = []ConnectorState{}
		}
		writeJSON(w, st)
	}))
	mux.HandleFunc("POST /channels/connect", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   string            `json:"name"`
			Fields map[string]string `json:"fields"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "bad request", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		if err := s.chans.ConnectChannel(ctx, req.Name, req.Fields); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"connected": req.Name})
	}))
	mux.HandleFunc("POST /channels/test", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "bad request", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := s.chans.TestChannel(ctx, req.Name); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"sent": req.Name})
	}))
	mux.HandleFunc("POST /channels/whatsapp/pair", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ByPhone bool              `json:"by_phone"`
			Fields  map[string]string `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if owner := req.Fields["owner"]; owner != "" {
			// Save the number first so pairing and the greeting know who you are.
			if err := s.chans.ConnectFields(r.Context(), "whatsapp", req.Fields); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		if err := s.chans.PairWhatsApp(r.Context(), req.ByPhone); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"pairing": "started"})
	}))
	mux.HandleFunc("GET /channels/whatsapp/pair", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		p := s.chans.WhatsAppPairing()
		if p == nil {
			writeJSON(w, map[string]string{"status": "none"})
			return
		}
		writeJSON(w, p)
	}))
	mux.HandleFunc("POST /channels/whatsapp/unpair", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		if err := s.chans.UnpairWhatsApp(r.Context()); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"unpaired": "whatsapp"})
	}))
	mux.HandleFunc("POST /channels/disconnect", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			http.Error(w, "bad request", 400)
			return
		}
		if err := s.chans.DisconnectChannel(r.Context(), req.Name); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"disconnected": req.Name})
	}))
}
