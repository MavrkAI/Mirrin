package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// "Why did you say that?" A reply on the screen on this computer has a Why?
// that lists the facts it drew on, each with a Forget (DELETE
// /memory/facts/{id}). It opens where the memory page does, on this computer
// only: what the twin knows about its owner stays here.

// WhyFact is a fact a reply drew on.
type WhyFact struct {
	ID      int64  `json:"id"`
	Subject string `json:"subject"`
	Content string `json:"content"`
	// How is "recalled" (the twin looked it up for this reply) or
	// "matched" (it was in mind as related to the message).
	How string `json:"how"`
}

// Why is what a reply drew on.
type Why struct {
	// Known is false when no note was kept for the reply: one from before
	// notes were kept, or one that wasn't a reply to the owner.
	Known bool `json:"known"`
	// Everything is true when every fact was in mind for the reply; Facts
	// are then the ones that matched most closely.
	Everything bool      `json:"everything"`
	Facts      []WhyFact `json:"facts"`
	// Lead is the sentence the screen shows above the facts.
	Lead string `json:"lead"`
}

// WhyTeller is a memory backend that can say what a reply drew on.
type WhyTeller interface {
	// Why is what the newest reply with these words drew on.
	Why(ctx context.Context, reply string) (Why, error)
}

// whyRoutes serves POST /memory/why ({"reply": "…"}), when the memory
// backend keeps notes.
func (s *Server) whyRoutes(mux *http.ServeMux, mem MemoryBackend, a authz) {
	wt, ok := mem.(WhyTeller)
	if !ok {
		return
	}
	mux.HandleFunc("POST /memory/why", a.Require(devices.Admin, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Reply string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Reply) == "" {
			s.fail(w, r, http.StatusBadRequest, apiError{Error: "no_reply", Message: "Tap Why? under one of my replies."})
			return
		}
		why, err := wt.Why(r.Context(), in.Reply)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, apiError{Error: "no_why", Message: "I couldn't look that up just now.", Fix: "Try again in a moment."})
			return
		}
		if why.Facts == nil {
			why.Facts = []WhyFact{}
		}
		why.Lead = whyLead(why)
		writeJSON(w, why)
	}))
}

// whyLead says plainly what went into a reply.
func whyLead(w Why) string {
	switch {
	case !w.Known:
		return "I didn't keep a note of what went into that one. Replies from before I started keeping track don't have one."
	case w.Everything && len(w.Facts) > 0:
		return "I had everything you've told me in mind; these matched most closely:"
	case w.Everything:
		return "I had everything you've told me in mind, and nothing in particular stood out for this one."
	case len(w.Facts) > 0:
		return "These are the things you've told me that I drew on:"
	}
	return "Nothing you've told me came into that one."
}
