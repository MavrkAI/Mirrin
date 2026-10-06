package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/devices"
)

// The limits change on this computer, within sense, and never from another
// device, even one with admin.
func TestSpendingLimitsChangeOnThisComputerOnly(t *testing.T) {
	e, f := pagesEnv(t)
	f.spending = SpendingInfo{Currency: "AUD", PerAction: 100, Monthly: 500}
	post := func(at where, auth, body string) *httptest.ResponseRecorder {
		return e.do(at, req{method: "POST", path: "/spending/limits", header: map[string]string{"Authorization": "Bearer " + auth, "Content-Type": "application/json"}, body: body})
	}
	w := post(onLoopback, master, `{"per_action_limit":1500,"monthly_limit":2000}`)
	var got SpendingInfo
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.PerAction != 1500 || got.Monthly != 2000 {
		t.Fatalf("raise: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{`{"per_action_limit":3000,"monthly_limit":2000}`, `{"per_action_limit":-1,"monthly_limit":2000}`,
		`{"per_action_limit":10,"monthly_limit":1e12}`, `{"monthly_limit":2000}`, `nonsense`} {
		if w := post(onLoopback, master, bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	e.s.AllowAdminRemote()
	_, admin, _ := e.store.Add("Phone", devices.KindPWA, devices.AllScopes, "tailscale", "")
	if w := post(onRemote, admin, `{"per_action_limit":99999,"monthly_limit":99999}`); w.Code == 200 {
		t.Fatalf("a phone raised the limits: %d", w.Code)
	}
	if f.spending.PerAction != 1500 {
		t.Fatalf("limits now %+v", f.spending)
	}
}
