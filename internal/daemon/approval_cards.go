package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// ScreenApproval is shared by the presence screen and the orb.
type ScreenApproval struct {
	ID          int64  `json:"id"`
	Summary     string `json:"summary"`
	Tool        string `json:"tool"`
	Chat        string `json:"chat"`
	Screenshot  string `json:"screenshot,omitempty"`
	CreatedAt   string `json:"created_at"`
	Risk        string `json:"risk"`
	Status      string `json:"status"`
	By          string `json:"by,omitempty"`
	AlwaysAllow bool   `json:"always_allow"`
	// Details are the call's arguments as labelled rows, when its summary
	// is only the bare "tool(arg=value)" a person can't read at a glance.
	Details []ApprovalDetail `json:"details,omitempty"`
	// Page: the request is about the page in the twin's browser (an act on
	// it, or a payment with a screenshot of it), so the screen brings the
	// page up with it.
	Page bool `json:"page,omitempty"`
}

// aboutThePage says whether an approval is about the page in the twin's
// browser: one of its browser tools, or a payment with a screenshot of the
// page it pays on. Anything else (an email with a screenshot in its chat,
// say) is not.
func aboutThePage(tool string, hasShot bool) bool {
	switch tool {
	case "browser_act", "browse_page", "browser_inspect", "screenshot_page", "browser_signin", "click":
		return true
	case "check_spend", "pay":
		return hasShot
	}
	return false
}

// ApprovalDetail is one labelled argument on an approval card.
type ApprovalDetail struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// approvalDetails lists a bare summary's arguments whole (what the yes is
// for must all be there), named the way a person would say them.
func approvalDetails(ap memory.Approval) []ApprovalDetail {
	if !bareSummary(ap) {
		return nil
	}
	var in map[string]any
	if json.Unmarshal(ap.Input, &in) != nil {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []ApprovalDetail
	for _, k := range keys {
		var v string
		switch x := in[k].(type) {
		case nil:
			continue
		case string:
			v = x
		case float64, bool:
			v = fmt.Sprint(x)
		default:
			b, _ := json.Marshal(x)
			v = string(b)
		}
		if strings.TrimSpace(v) == "" {
			continue
		}
		name := strings.ReplaceAll(k, "_", " ")
		out = append(out, ApprovalDetail{Name: strings.ToUpper(name[:1]) + name[1:], Value: v})
	}
	return out
}

func (d *Daemon) approvalCard(ctx context.Context, ap memory.Approval) ScreenApproval {
	p := api.PeerFrom(ctx)
	card := ScreenApproval{ID: ap.ID, Summary: ap.Summary, Tool: ap.Tool,
		Chat: ap.ChatKey, CreatedAt: ap.CreatedAt.In(d.location()).Format(time.RFC3339),
		Risk: ap.Risk.String(), Status: ap.Status, By: ap.DecidedBy,
		AlwaysAllow: ap.Status == "pending" && approvals.SafetyFloor(ap.Tool, ap.Risk) == "" && !p.Lacks(devices.Approve) && !p.Lacks(devices.Chat) &&
			d.answerable("screen:local", ap) && d.alwaysBlocked(ctx, "screen:local", ap) == "",
		Details: approvalDetails(ap)}
	if ap.Status == "pending" {
		if path := d.approvalScreenshot(ctx, ap); path != "" {
			card.Screenshot = "/screen/shot?path=" + url.QueryEscape(path)
		}
	}
	card.Page = ap.Status == "pending" && aboutThePage(ap.Tool, card.Screenshot != "")
	return card
}

func (d *Daemon) screenApprovals(ctx context.Context) []ScreenApproval {
	cards := []ScreenApproval{}
	pending, _ := d.store.AllPendingApprovals(ctx)
	seen := map[int64]bool{}
	for _, ap := range pending {
		cards = append(cards, d.approvalCard(ctx, ap))
		seen[ap.ID] = true
	}
	// Retain recent decisions so the cards show who answered, including a
	// decision on another device. Read the stored row as the source of truth.
	recent := d.bus.Recent()
	decided := 0
	for i := len(recent) - 1; i >= 0 && decided < 5; i-- {
		news, ok := recent[i].Data.(approvalNews)
		if recent[i].Kind != "approval" || !ok || seen[news.ID] {
			continue
		}
		seen[news.ID] = true
		ap, err := d.store.GetApproval(ctx, news.ID)
		if err != nil || ap.Status == "pending" {
			continue
		}
		cards = append(cards, d.approvalCard(ctx, *ap))
		decided++
	}
	return cards
}
