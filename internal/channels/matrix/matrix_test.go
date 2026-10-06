package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels"
)

func TestHandleSync(t *testing.T) {
	joined := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && len(r.URL.Path) > 24 && r.URL.Path[:24] == "/_matrix/client/v3/join/" {
			joined[r.URL.Path[24:]] = true
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "@mavrk:hs", "tok", "@akshay:hs", false, nil)
	var got []channels.Inbound
	h := func(_ context.Context, in channels.Inbound) { got = append(got, in) }
	now := time.Now().UnixMilli()
	var res syncResponse
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"next_batch":"s2","rooms":{
	  "invite":{"!inv:hs":{"invite_state":{"events":[{"type":"m.room.member","sender":"@akshay:hs","state_key":"@mavrk:hs","content":{"membership":"invite"}}]}},
	            "!bad:hs":{"invite_state":{"events":[{"type":"m.room.member","sender":"@spammer:hs","state_key":"@mavrk:hs","content":{"membership":"invite"}}]}}},
	  "join":{"!dm:hs":{"timeline":{"events":[
	    {"type":"m.room.message","sender":"@akshay:hs","origin_server_ts":%d,"content":{"msgtype":"m.text","body":"hello"}},
	    {"type":"m.room.message","sender":"@mavrk:hs","origin_server_ts":%d,"content":{"msgtype":"m.text","body":"own echo"}},
	    {"type":"m.room.message","sender":"@other:hs","origin_server_ts":%d,"content":{"msgtype":"m.text","body":"stranger"}},
	    {"type":"m.room.message","sender":"@akshay:hs","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"old backlog"}}
	  ]}}}}}`, now, now, now)), &res)
	c.handleSync(context.Background(), res, h)
	if len(got) != 1 || got[0].Text != "hello" || !got[0].IsOwner || c.OwnerChatID() != "!dm:hs" {
		t.Fatalf("got %+v", got)
	}
	if !joined["!inv:hs"] || joined["!bad:hs"] {
		t.Fatalf("join decisions wrong: %v", joined)
	}
}
