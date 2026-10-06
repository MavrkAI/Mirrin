package google

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// ErrNotConnected is returned before anyone has signed in.
var ErrNotConnected = errors.New("Google isn't connected yet. The owner can connect it from Accounts in the Mirrin menu")

// SignOut is Google refusing the stored sign-in. For a personal account
// this happens most often because the owner's Google project is still in
// Testing, where Google ends every sign-in after 7 days; also when access
// is removed from the Google account, or after a password change.
type SignOut struct {
	At time.Time
	// Client is set when Google refused the OAuth client itself (deleted,
	// or its secret changed), so a new client is needed, not just a new
	// sign-in.
	Client bool
	// Detail is Google's own words.
	Detail string
	// Token names the refused sign-in (no part of the token itself).
	Token string
}

// signedOutError is what every Google call returns once Google has refused
// the sign-in: the same plain words each time, meant to be passed on.
type signedOutError struct{ so SignOut }

func (e *signedOutError) Error() string {
	if e.so.Client {
		return "Google no longer accepts Mirrin's OAuth client (it was deleted, or its secret changed), so Calendar, Gmail and Drive can't be reached. The owner needs to open Accounts in the Mirrin menu, paste a new Desktop app client (step 5) and press Connect Google"
	}
	return "Google has signed Mirrin out, so Calendar, Gmail and Drive can't be reached until the owner reconnects: Accounts in the Mirrin menu → Connect Google. If their Google project is still in Testing, Google does this every 7 days; publishing the app (Google Auth Platform → Audience → Publish app) stops it"
}

// IsSignedOut reports whether err means Google refused the stored sign-in,
// and how.
func IsSignedOut(err error) (SignOut, bool) {
	var e *signedOutError
	if errors.As(err, &e) {
		return e.so, true
	}
	return SignOut{}, false
}

// offlineError is a sign-in that couldn't be renewed because Google
// couldn't be reached: nothing is wrong with the sign-in itself.
type offlineError struct{ err error }

func (e *offlineError) Error() string {
	return "couldn't reach Google to renew the sign-in (" + short(e.err) + "); it will try again"
}
func (e *offlineError) Unwrap() error { return e.err }

// IsOffline reports whether err is Google not answering.
func IsOffline(err error) bool {
	var e *offlineError
	return errors.As(err, &e)
}

func short(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// classify recognises a refusal from Google's token endpoint.
func classify(err error) (SignOut, bool) {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		code := re.ErrorCode
		if code == "" {
			for _, c := range []string{"invalid_grant", "deleted_client", "invalid_client", "unauthorized_client"} {
				if strings.Contains(string(re.Body), c) {
					code = c
					break
				}
			}
		}
		switch code {
		case "invalid_grant":
			return SignOut{Detail: or(re.ErrorDescription, "Token has been expired or revoked.")}, true
		case "unauthorized_client":
			// The sign-in belongs to another client (the client was
			// swapped after connecting): a new sign-in is all it needs.
			return SignOut{Detail: or(re.ErrorDescription, code)}, true
		case "invalid_client", "deleted_client":
			return SignOut{Client: true, Detail: or(re.ErrorDescription, code)}, true
		}
		return SignOut{}, false
	}
	if strings.Contains(err.Error(), "refresh token is not set") {
		return SignOut{Detail: "the stored sign-in can't be renewed"}, true
	}
	return SignOut{}, false
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// exchangeError puts a failed code exchange in words the owner can act on.
func (a *Auth) exchangeError(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return fmt.Errorf("couldn't reach Google to finish signing in (%s); press Connect Google to try again", short(err))
	}
	switch re.ErrorCode {
	case "invalid_client", "unauthorized_client", "deleted_client":
		a.setAside()
		return errors.New("Google didn't accept the Client ID and secret. Copy them again from Google Auth Platform → Clients (or download the JSON), paste them on the Accounts page, and connect again")
	case "invalid_grant":
		return errors.New("that sign-in code had expired or was already used; press Connect Google to try again")
	case "redirect_uri_mismatch":
		return errors.New("Google refused the return address. Use a Desktop app client (Google Auth Platform → Clients → Create client → Desktop app) and connect from the computer running Mirrin")
	}
	return fmt.Errorf("Google didn't finish the sign-in (%s); press Connect Google to try again", or(re.ErrorDescription, re.ErrorCode))
}

// ExplainCallback says, for the page Google sends the browser back to, why
// the sign-in stopped. code is the error Google put on the callback.
func ExplainCallback(code string) string {
	switch code {
	case "access_denied":
		return "The sign-in didn't go through: either Cancel was pressed, or Google blocked it because the app is still in Testing and this account isn't one of its test users. In Google Auth Platform → Audience, press Publish app (or add your address under Test users), then press Connect Google again."
	case "admin_policy_enforced":
		return "Your Google Workspace admin doesn't allow apps like this one. Ask them to allow it, or connect a personal Google account."
	case "org_internal":
		return "This app is set to Internal, so only accounts in its Google Workspace organisation can sign in. Sign in with one of those, or switch the Audience to External."
	case "invalid_scope":
		return "Google refused what Mirrin asked to see. Make sure the Calendar, Gmail and Drive APIs are turned on for your project, then press Connect Google again."
	case "redirect_uri_mismatch", "invalid_request":
		return "Google refused the return address. Use a Desktop app client (Google Auth Platform → Clients → Create client → Desktop app) and connect from the computer running Mirrin."
	}
	if !reErrorCode.MatchString(code) {
		// The page shows this; only a plain error code is repeated, never
		// whatever else a link put in the address.
		return "Google stopped the sign-in with an error it didn't name. Press Connect Google on the Accounts page to try again."
	}
	return "Google stopped the sign-in (" + code + "). Press Connect Google on the Accounts page to try again."
}

// reErrorCode is what an OAuth error code looks like (RFC 6749: access_denied).
var reErrorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

var apiLabel = map[string]string{"calendar": "Calendar", "gmail": "Gmail", "drive": "Drive"}

func label(api string) string {
	if l, ok := apiLabel[api]; ok {
		return l
	}
	return api
}

// APIURL is where the owner turns an API on for their Google project.
var APIURL = map[string]string{
	"calendar": "https://console.cloud.google.com/apis/library/calendar-json.googleapis.com",
	"gmail":    "https://console.cloud.google.com/apis/library/gmail.googleapis.com",
	"drive":    "https://console.cloud.google.com/apis/library/drive.googleapis.com",
}

var reEnableURL = regexp.MustCompile(`https://console\.(?:developers|cloud)\.google\.com/apis/api/[^\s"']+`)

// Explain turns a failed call to one of Google's APIs into words the model
// can pass on, and remembers lasting problems (an API turned off, a box left
// unticked) for the Accounts page and the self-check.
func (a *Auth) Explain(api string, err error) error {
	if err == nil {
		return nil
	}
	if so, ok := IsSignedOut(err); ok {
		return &signedOutError{so}
	}
	if IsOffline(err) || errors.Is(err, ErrNotConnected) {
		var off *offlineError
		if errors.As(err, &off) {
			return off
		}
		return ErrNotConnected
	}
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return err
	}
	reasons := apiReasons(ge)
	has := func(rs ...string) bool {
		for _, r := range rs {
			if reasons[r] {
				return true
			}
		}
		return false
	}
	msg := ge.Message
	switch {
	case has("accessNotConfigured", "SERVICE_DISABLED") || strings.Contains(msg, "has not been used in project") || strings.Contains(msg, "it is disabled"):
		link := APIURL[api]
		if m := reEnableURL.FindString(msg); m != "" {
			link = m
		}
		p := fmt.Sprintf("The %s API is turned off for your Google Cloud project. Turn it on at %s (press Enable), wait a minute, and it will work.", label(api), link)
		a.setProblem(api, p)
		return errors.New(p)
	case has("insufficientPermissions", "ACCESS_TOKEN_SCOPE_INSUFFICIENT") || strings.Contains(msg, "insufficient authentication scopes"):
		p := fmt.Sprintf("%s wasn't ticked when Google asked for permission. To add it, open Accounts, press Disconnect, then Connect Google and tick every box.", label(api))
		a.setProblem(api, p)
		return errors.New(p)
	case ge.Code == 401:
		a.expireAccess()
		return errors.New("Google refused the sign-in for that request; try once more (if Google has signed Mirrin out, the owner will be told how to reconnect)")
	case ge.Code == 429 || has("rateLimitExceeded", "userRateLimitExceeded", "RATE_LIMIT_EXCEEDED"):
		return fmt.Errorf("Google says there have been too many %s requests just now; try again in a minute", label(api))
	case ge.Code == 404:
		return fmt.Errorf("Google couldn't find that in %s (it may have been deleted or moved)", label(api))
	case ge.Code >= 500:
		return fmt.Errorf("%s is having trouble on Google's side (%d); try again shortly", label(api), ge.Code)
	}
	return fmt.Errorf("%s said: %s", label(api), or(msg, ge.Error()))
}

// apiReasons collects the machine-readable reasons in a Google API error.
func apiReasons(ge *googleapi.Error) map[string]bool {
	out := map[string]bool{}
	for _, e := range ge.Errors {
		out[e.Reason] = true
	}
	for _, d := range ge.Details {
		if m, ok := d.(map[string]any); ok {
			if r, ok := m["reason"].(string); ok {
				out[r] = true
			}
		}
	}
	if ge.Body != "" {
		for _, r := range []string{"SERVICE_DISABLED", "ACCESS_TOKEN_SCOPE_INSUFFICIENT", "accessNotConfigured", "insufficientPermissions"} {
			if strings.Contains(ge.Body, r) {
				out[r] = true
			}
		}
	}
	return out
}

func (a *Auth) setProblem(api, p string) {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	a.setProblemLocked(api, p)
}

func (a *Auth) setProblemLocked(api, p string) {
	if a.problems == nil {
		a.problems = map[string]string{}
	}
	a.problems[api] = p
}

// Problems are what is known to be wrong with each feature (an API turned
// off, a box left unticked), from the last sign-in, probe or call.
func (a *Auth) Problems() map[string]string {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	out := make(map[string]string, len(a.problems))
	for k, v := range a.problems {
		out[k] = v
	}
	return out
}
