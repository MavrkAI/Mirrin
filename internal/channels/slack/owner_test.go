package slack

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

const sharedDisplayName = `{"ok":true,"members":[
	{"id":"U0FIRST001","name":"akshay.k","profile":{"display_name":"akshay"}},
	{"id":"U0SECOND02","name":"akshay.m","profile":{"display_name":"akshay"}}
],"response_metadata":{"next_cursor":""}}`

func TestOwnerIsNotAdoptedByDisplayName(t *testing.T) {
	srv, _ := fakeSlack(t, func(method string) string {
		if method == "users.list" {
			return sharedDisplayName
		}
		return `{"ok":true}`
	})
	c := New("xoxb", "xapp", "akshay", false, nil)
	c.api = srv.URL + "/"
	c.resolveOwner(context.Background())
	if c.ownerID != "" {
		t.Fatalf("a shared display name adopted %q as the owner", c.ownerID)
	}

	c = New("xoxb", "xapp", "@akshay.m", false, nil)
	c.api = srv.URL + "/"
	c.resolveOwner(context.Background())
	if c.ownerID != "U0SECOND02" {
		t.Fatalf("the unique handle should resolve, got %q", c.ownerID)
	}

	c = New("xoxb", "xapp", "U0FIRST001", false, nil)
	if c.ownerID != "U0FIRST001" {
		t.Fatalf("a member id should resolve without a lookup, got %q", c.ownerID)
	}
}

// When the lookup at connect settles on nobody (the handle is shared) a
// sender whose handle matches is not adopted as the owner later; when the
// lookup failed, a matching sender makes it look again and only the member
// users.list names becomes the owner.
func TestOwnerIsNotAdoptedFromAHandleAfterTheLookup(t *testing.T) {
	var listFails atomic.Bool
	srv, _ := fakeSlack(t, func(method string) string {
		switch method {
		case "users.list":
			if listFails.Load() {
				return `{"ok":false,"error":"ratelimited"}`
			}
			return `{"ok":true,"members":[
				{"id":"U0OWNER001","name":"akshay"},
				{"id":"U0GUEST002","name":"akshay"}
			],"response_metadata":{"next_cursor":""}}`
		case "users.info":
			return `{"ok":true,"user":{"name":"akshay"}}`
		}
		return `{"ok":true}`
	})
	event := func(user, ts string) envelope {
		var env envelope
		_ = json.Unmarshal([]byte(`{"type":"events_api","payload":{"event":{"type":"message","channel_type":"im","channel":"D1","user":"`+user+`","text":"hi","ts":"`+ts+`"}}}`), &env)
		return env
	}
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }

	c := New("xoxb", "xapp", "@akshay", true, nil)
	c.api = srv.URL + "/"
	c.selfID = "UBOT"
	c.resolveOwner(context.Background())
	c.onEvent(context.Background(), event("U0GUEST002", "9999999999.0001"), h)
	if c.ownerID != "" || len(got) != 1 || got[0].IsOwner {
		t.Fatalf("adopted %q as owner: %+v", c.ownerID, got)
	}

	listFails.Store(true)
	got = nil
	c = New("xoxb", "xapp", "@akshay", true, nil)
	c.api = srv.URL + "/"
	c.selfID = "UBOT"
	c.resolveOwner(context.Background())
	c.onEvent(context.Background(), event("U0GUEST002", "9999999999.0002"), h)
	if c.ownerID != "" || len(got) != 1 || got[0].IsOwner {
		t.Fatalf("adopted %q as owner while users.list failed: %+v", c.ownerID, got)
	}
}
