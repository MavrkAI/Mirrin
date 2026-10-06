package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/push"
	"github.com/MavrkAI/Mirrin/internal/tasks"
)

func (d *Daemon) attachPush(ctx context.Context, srv *api.Server) {
	c := d.Config()
	if !c.Push.IsEnabled() {
		return
	}
	key, err := push.LoadOrCreateVAPID(filepath.Join(c.DataDir, "vapid.pem"))
	if err != nil {
		d.log.Warn("notifications unavailable: couldn't load the push key")
		return
	}
	store, err := push.Open(filepath.Join(c.DataDir, "push.json"))
	if err != nil {
		d.log.Warn("notifications unavailable: couldn't load subscriptions")
		return
	}
	sender := &push.Sender{VAPID: key, Subject: c.Push.Subject, Client: push.NewClient(nil)}
	dispatch := push.NewDispatcher(store, func(ctx context.Context, sub push.Subscription, b []byte, kind, tag string) error {
		current := *sender
		current.Subject = d.Config().Push.Subject
		return current.Send(ctx, sub, b, kind, tag)
	})
	dispatch.Log = d.log
	dispatch.Settings = func() (string, push.Config) { cfg := d.Config(); return cfg.Name, cfg.Push }
	dispatch.Active = func(id string) bool {
		dev, ok := srv.Devices().Get(id)
		return ok && !dev.Revoked() && (dev.Has(devices.View) || dev.Has(devices.Approve))
	}
	dispatch.Approves = func(id string) bool { return approves(srv.Devices(), id) }
	for _, sub := range store.List() {
		if !dispatch.Active(sub.DeviceID) {
			_ = store.Delete(sub.DeviceID, "")
		}
	}
	srv.WithPush(key, store, dispatch, nil)
	pushDispatchers.Store(d, dispatch)
	context.AfterFunc(ctx, func() { pushDispatchers.CompareAndDelete(d, dispatch) })
	d.wirePushApprovals(ctx, dispatch)
	go dispatch.Run(ctx)
}

var pushDispatchers sync.Map // *Daemon -> *push.Dispatcher, only while running

// approves reports whether device id may act for the owner: paired with
// Approve scope and not revoked. A wall screen paired only to look may not.
func approves(devs *devices.Store, id string) bool {
	dev, ok := devs.Get(id)
	return ok && !dev.Revoked() && dev.Has(devices.Approve)
}

func (d *Daemon) wirePushApprovals(ctx context.Context, dispatch *push.Dispatcher) {
	d.agent.OnApproval(func(_ context.Context, e agent.ApprovalEvent) {
		aps, err := d.store.AllPendingApprovals(ctx)
		if err != nil {
			return
		}
		name := d.Config().Name
		exclude := ""
		if start := strings.LastIndex(e.By, "["); start >= 0 {
			if end := strings.Index(e.By[start:], "]"); end >= 0 {
				exclude = e.By[start+1 : start+end]
			}
		}
		dispatch.Enqueue(push.Approval(e.ID, e.Summary, e.Status, name, len(aps)), exclude)
	})
}

// PushSecurity is the direct notification hook for certificate/reach alarms.
// The alarm producer remains responsible for auditing and the local warning.
func (d *Daemon) PushSecurity(id, summary string) {
	if p, ok := pushDispatchers.Load(d); ok {
		name := d.Config().Name
		sum := sha256.Sum256([]byte(id))
		p.(*push.Dispatcher).Enqueue(push.Notification{Kind: "security", Title: name + " needs you", Body: summary, URL: "/ui", Tag: fmt.Sprintf("security-%x", sum[:8])}, "")
	}
}

// pushHandOver tells the owner's phone that a page in the twin's browser is
// waiting for them, and says whether any phone was told. Only a device that
// can approve hears it. Taking over happens on this computer, so a tap opens
// the screen with the page up (/ui#browser) and what it needs.
func (d *Daemon) pushHandOver() bool {
	p, ok := pushDispatchers.Load(d)
	if !ok {
		return false
	}
	n := push.Notification{Kind: "handover", Title: d.Config().Name + " needs you in the browser", Body: "Tap to see what it needs.", URL: "/ui#browser", Tag: "handover"}
	return p.(*push.Dispatcher).Enqueue(n, "") > 0
}

func questionPush(list []tasks.Task, key, text, name string) (push.Notification, bool) {
	for _, t := range list {
		if t.Status == tasks.WaitingUser && t.Owner == key && text == t.Title+": "+t.Question {
			return push.Notification{Kind: "question", Title: name + " has a question", Body: t.Question, URL: "/ui", Tag: "question-" + t.ID}, true
		}
	}
	return push.Notification{}, false
}
func (d *Daemon) notifyTaskWithPush(ctx context.Context, key, text string) error {
	if p, ok := pushDispatchers.Load(d); ok && d.tasks != nil {
		if n, ok := questionPush(d.tasks.List(), key, text, d.Config().Name); ok {
			p.(*push.Dispatcher).Enqueue(n, "")
		}
	}
	return d.Notify(ctx, key, text)
}
