package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/push"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

func TestPushApprovalDecisionAndBadge(t *testing.T) {
	td := newTestDaemon(t, butler)
	store, _ := push.Open("")
	sub := push.Subscription{Endpoint: "https://fcm.googleapis.com/test", DeviceID: "other-phone"}
	sub.Keys.P256DH = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	sub.Keys.Auth = "BTBZMqHH6r4Tts7J_aSIgg"
	if err := store.Put(sub); err != nil {
		t.Fatal(err)
	}
	heard := make(chan map[string]any, 8)
	dispatch := push.NewDispatcher(store, func(_ context.Context, _ push.Subscription, b []byte, _, _ string) error {
		var p map[string]any
		json.Unmarshal(b, &p)
		heard <- p
		return nil
	})
	dispatch.Settings = func() (string, push.Config) { return td.Config().Name, push.Config{Preview: "private"} }
	dispatch.Approves = func(string) bool { return true }
	td.wirePushApprovals(t.Context(), dispatch)
	go dispatch.Run(t.Context())
	td.owner(t, "email the boss")
	wait := func(kind string) map[string]any {
		t.Helper()
		select {
		case p := <-heard:
			if p["k"] != kind {
				t.Fatal(p)
			}
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("push not delivered")
			return nil
		}
	}
	pending := wait("approval")
	if pending["badge"] != float64(1) {
		t.Fatal(pending)
	}
	if _, err := td.DecideApprovalBy(t.Context(), 1, false, Decider{DeviceID: "deciding-phone", Method: "screen", Via: "loopback"}); err != nil {
		t.Fatal(err)
	}
	resolved := wait("resolved")
	if resolved["tag"] != pending["tag"] || resolved["badge"] != float64(0) || resolved["b"] != "This request no longer needs you." {
		t.Fatal(resolved)
	}
}
func TestPushQuestionSelection(t *testing.T) {
	task := tasks.Task{ID: "12", Owner: "cli:owner", Title: "Trip", Question: "Which day?", Status: tasks.WaitingUser}
	n, ok := questionPush([]tasks.Task{task}, task.Owner, "Trip: Which day?", "Ava")
	if !ok || n.Kind != "question" || n.Body != task.Question {
		t.Fatal(n, ok)
	}
	if _, ok = questionPush([]tasks.Task{task}, "other", "Trip: Which day?", "Ava"); ok {
		t.Fatal("other chat's question sent")
	}
}

func TestPushSecurityDirectHook(t *testing.T) {
	td := newTestDaemon(t, butler)
	store, _ := push.Open("")
	sub := push.Subscription{Endpoint: "https://fcm.googleapis.com/test", DeviceID: "phone", Origin: "https://twin.test"}
	sub.Keys.P256DH = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	sub.Keys.Auth = "BTBZMqHH6r4Tts7J_aSIgg"
	if err := store.Put(sub); err != nil {
		t.Fatal(err)
	}
	heard := make(chan string, 1)
	d := push.NewDispatcher(store, func(_ context.Context, _ push.Subscription, _ []byte, kind, _ string) error {
		heard <- kind
		return nil
	})
	pushDispatchers.Store(td.Daemon, d)
	defer pushDispatchers.Delete(td.Daemon)
	go d.Run(t.Context())
	td.PushSecurity("certificate", "Check the certificate on your computer.")
	select {
	case kind := <-heard:
		if kind != "security" {
			t.Fatal(kind)
		}
	case <-time.After(time.Second):
		t.Fatal("security hook lost notification")
	}
}

func TestPushConfigFollowsFeatureSections(t *testing.T) {
	typ := reflect.TypeFor[config.Config]()
	name, _ := typ.FieldByName("Name")
	ui, _ := typ.FieldByName("UI")
	field, _ := typ.FieldByName("Push")
	if name.Index[0] != 0 || field.Index[0] <= ui.Index[0] {
		t.Fatal("push must follow the identity and feature settings")
	}
}
