package google

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
)

// clientSuffix ends every Google OAuth client ID.
const clientSuffix = ".apps.googleusercontent.com"

// WriteClient saves an OAuth client id and secret as a credentials file
// (the "Desktop app" shape Google's console produces). It catches the usual
// slips: the two fields swapped, quotes copied along, an API key or the
// whole JSON pasted into the ID box.
func (a *Auth) WriteClient(clientID, clientSecret string) error {
	clientID, clientSecret = unquote(clientID), unquote(clientSecret)
	if strings.HasPrefix(clientID, "{") {
		return a.ImportCredentials([]byte(clientID)) // the whole downloaded file, in the ID box
	}
	if strings.HasSuffix(clientSecret, clientSuffix) && !strings.HasSuffix(clientID, clientSuffix) {
		clientID, clientSecret = clientSecret, clientID // swapped: fine, we know which is which
	}
	switch {
	case clientID == "" || clientSecret == "":
		return errors.New("both the Client ID and the Client secret are needed; they're on the client's page in Google Auth Platform → Clients")
	case strings.HasPrefix(clientID, "AIza"):
		return errors.New("that's an API key, not an OAuth client. In Google Auth Platform → Clients, create a Desktop app client and copy its Client ID (it ends in .apps.googleusercontent.com)")
	case !strings.HasSuffix(clientID, clientSuffix):
		return errors.New("that doesn't look like a Client ID: it ends in .apps.googleusercontent.com. Copy it from Google Auth Platform → Clients")
	case strings.HasSuffix(clientSecret, clientSuffix):
		return errors.New("the Client secret box has a Client ID in it; the secret usually starts with GOCSPX-")
	}
	b, _ := json.Marshal(map[string]any{"installed": map[string]any{
		"client_id": clientID, "client_secret": clientSecret, "project_id": "mirrin",
		"auth_uri": "https://accounts.google.com/o/oauth2/auth", "token_uri": "https://oauth2.googleapis.com/token",
		"redirect_uris": []string{"http://localhost"},
	}})
	return os.WriteFile(a.CredentialsFile, b, 0o600)
}

func unquote(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"'`+"`"))
}

// clientFile is the part of a credentials JSON the twin cares about.
type clientFile struct {
	Type      string       `json:"type"`
	Installed *clientBlock `json:"installed"`
	Web       *clientBlock `json:"web"`
}

type clientBlock struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	RedirectURIs []string `json:"redirect_uris"`
}

// ImportCredentials saves a credentials.json downloaded from Google as-is,
// after checking it is an OAuth client the twin can use.
func (a *Auth) ImportCredentials(data []byte) error {
	var f clientFile
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &f); err != nil {
		return errors.New("that isn't the JSON file Google gives you. In Google Auth Platform → Clients, press the download button next to your Desktop client and paste the whole file")
	}
	switch {
	case f.Type == "service_account":
		return errors.New("that's a service account key. Mirrin signs in as you, which needs an OAuth client: Google Auth Platform → Clients → Create client → Desktop app")
	case f.Installed == nil && f.Web == nil:
		return errors.New("credentials JSON needs an \"installed\" or \"web\" client; download it from Google Auth Platform → Clients")
	}
	c := f.Installed
	if c == nil {
		c = f.Web
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return errors.New("that file has no client secret in it. Download the JSON again from Google Auth Platform → Clients (the secret is only in a fresh download)")
	}
	if !strings.HasSuffix(c.ClientID, clientSuffix) {
		return errors.New("that file's client_id isn't a Google OAuth client ID; download the JSON from Google Auth Platform → Clients")
	}
	return os.WriteFile(a.CredentialsFile, data, 0o600)
}

// checkRedirect makes sure Google will send the sign-in back where it has to
// go. A Desktop app client takes any address on this computer; a Web
// application client only the addresses listed on it.
func (a *Auth) checkRedirect(redirect string) error {
	b, err := os.ReadFile(a.CredentialsFile)
	if err != nil {
		return nil
	}
	var f clientFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	if f.Web != nil && f.Installed == nil {
		if slices.Contains(f.Web.RedirectURIs, redirect) {
			return nil
		}
		if !a.Connected() {
			// Set aside so the page asks for a new client. A sign-in made
			// with this one still works (renewing it needs no return
			// address), so while connected the client stays.
			a.setAside()
		}
		return fmt.Errorf("that client is a Web application, which only returns to addresses listed on it. Easiest: create a Desktop app client instead (Google Auth Platform → Clients → Create client → Desktop app) and paste it here. Or add %s under Authorised redirect URIs on the client, then paste the JSON again", redirect)
	}
	u, err := url.Parse(redirect)
	if err != nil || !loopback(u.Hostname()) {
		return errors.New("Google only sends a sign-in back to the computer it started on. Open Accounts on the computer running Mirrin (from its menu) and connect there")
	}
	return nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// setAside moves a client Google won't work with out of the way (kept as
// .rejected, in case), so the Accounts page asks for a new one.
func (a *Auth) setAside() {
	_ = os.Rename(a.CredentialsFile, a.CredentialsFile+".rejected")
}
