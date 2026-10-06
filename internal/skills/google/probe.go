package google

import (
	"context"
	"net/http"
	"time"

	"google.golang.org/api/googleapi"
)

// probeURL is the smallest call each API answers.
var probeURL = map[string]string{
	"calendar": "https://www.googleapis.com/calendar/v3/users/me/calendarList?maxResults=1&fields=items(id)",
	"gmail":    "https://gmail.googleapis.com/gmail/v1/users/me/profile",
	"drive":    "https://www.googleapis.com/drive/v3/about?fields=user(emailAddress)",
}

// Probe makes sure the sign-in works and tries each named API once. It
// returns what is wrong, by API (an API turned off for the project, a box
// left unticked), and remembers it for Problems. The error is for the sign-in
// itself: not connected, signed out, or Google out of reach.
func (a *Auth) Probe(ctx context.Context, apis ...string) (map[string]string, error) {
	if err := a.Check(ctx); err != nil {
		return nil, err
	}
	hc, err := a.HTTPClient()
	if err != nil {
		return nil, err
	}
	found := map[string]string{}
	for _, api := range apis {
		u, ok := probeURL[api]
		if !ok {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, _ := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
		resp, err := hc.Do(req)
		if err != nil {
			cancel()
			if _, out := IsSignedOut(err); out {
				return nil, a.Explain(api, err)
			}
			return nil, &offlineError{err}
		}
		err = googleapi.CheckResponse(resp)
		resp.Body.Close()
		cancel()
		if err == nil {
			continue
		}
		// Only lasting problems are the owner's to fix: Explain records the
		// known ones; a refusal it doesn't know is reported as Google said
		// it. Google's hiccups (a 5xx, a rate limit) and a refused access
		// token (renewed on the next call) are not the owner's.
		e := a.Explain(api, err)
		if p, ok := a.Problems()[api]; ok {
			found[api] = p
		} else if ge, ok := err.(*googleapi.Error); ok && ge.Code >= 400 && ge.Code < 500 && ge.Code != 401 && ge.Code != 404 && ge.Code != 429 {
			found[api] = e.Error()
		}
	}
	a.tmu.Lock()
	for _, api := range apis {
		if p, ok := found[api]; ok {
			a.setProblemLocked(api, p)
		} else if a.granted == nil || a.granted[api] {
			delete(a.problems, api) // works now
		}
	}
	a.tmu.Unlock()
	return found, nil
}
