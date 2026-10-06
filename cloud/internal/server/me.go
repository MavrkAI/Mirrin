package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
)

// Columns shown as times or booleans; every other column is shown as
// stored. Times are Unix seconds in the store and RFC 3339 here, with 0 as
// null.
var (
	timeColumns = []string{"paid_through", "billing_at", "delete_at", "created", "gen_at", "released", "expires", "at", "occurred"}
	boolColumns = []string{"revoked", "byod", "dns_pending"}
)

// me answers GET /v1/me: every row the control plane keeps that names this
// account, every column of each, found from the schema itself so a new
// table or column cannot be left out; the deny-list rows for its keys and
// handle, every column too; and the email, fetched from the merchant of
// record for this answer and never stored.
func (s *Server) me(w http.ResponseWriter, r *http.Request, d store.Device, _ []byte) {
	doc, customer, err := accountDoc(r.Context(), s.store, d.Account, s.now().UTC())
	if err != nil {
		internal(w, s, err)
		return
	}
	doc["email"] = nil
	if email, err := s.billing.Email(r.Context(), customer); err == nil {
		doc["email"] = email
	} else {
		s.log.Warn("email for /v1/me", "err", err)
	}
	reply(w, http.StatusOK, doc)
}

// accountDoc is /v1/me without the email, and the billing customer to fetch
// it for.
func accountDoc(ctx context.Context, st *store.Store, account string, now time.Time) (map[string]any, string, error) {
	doc := map[string]any{"namespaces": []any{}}
	var customer string
	err := st.View(ctx, func(tx *store.Tx) error {
		a, err := tx.Account(account)
		if err != nil {
			return err
		}
		customer = a.BillingCustomer
		tables, err := tx.Schema()
		if err != nil {
			return err
		}
		for _, tb := range tables {
			if !store.AccountLinked(tb) {
				continue
			}
			col := "account"
			if tb.Name == "accounts" {
				col = "id"
			}
			rows, err := tx.Rows(tb.Name, col, account)
			if err != nil {
				return err
			}
			if tb.Name == "accounts" {
				if len(rows) != 1 {
					return fmt.Errorf("account %s: %d rows", account, len(rows))
				}
				acct := render(rows[0])
				acct["status"] = status(a, now)
				doc["account"] = acct
				continue
			}
			doc[tb.Name] = renderAll(rows)
		}
		// Links a device of this account started and nobody paid.
		devs, err := tx.Devices(account)
		if err != nil {
			return err
		}
		links, _ := doc["links"].([]map[string]any)
		deny := []map[string]any{}
		for _, d := range devs {
			ls, err := tx.Rows("links", "key_id", d.KeyID)
			if err != nil {
				return err
			}
			for _, l := range renderAll(ls) {
				if !slices.ContainsFunc(links, func(x map[string]any) bool { return x["id"] == l["id"] }) {
					links = append(links, l)
				}
			}
			if deny, err = appendDeny(tx, deny, store.DenyKey, d.Pub); err != nil {
				return err
			}
		}
		if links != nil {
			doc["links"] = links
		}
		if h, err := tx.AccountHandle(account); err == nil {
			if deny, err = appendDeny(tx, deny, store.DenyHandle, h.Name); err != nil {
				return err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		doc["deny"] = deny
		// The backup objects of its namespaces: names and sizes only.
		objs := []map[string]any{}
		nss, err := tx.AccountNamespaces(account)
		if err != nil {
			return err
		}
		for _, n := range nss {
			rows, err := tx.Rows("objects", "ns", n.NS)
			if err != nil {
				return err
			}
			objs = append(objs, renderAll(rows)...)
		}
		doc["objects"] = objs
		doc["deletion"] = nil
		if !a.DeleteAt.IsZero() {
			doc["deletion"] = map[string]any{"delete_at": a.DeleteAt.Format(time.RFC3339)}
		}
		return nil
	})
	return doc, customer, err
}

// status is the account as the daemon sees it: deleting, lapsed or active.
// billing_status is the subscription as the merchant of record reports it.
func status(a store.Account, now time.Time) string {
	switch {
	case !a.DeleteAt.IsZero():
		return "deleting"
	case !now.Before(a.PaidThrough):
		return "lapsed"
	}
	return "active"
}

func render(r store.Row) map[string]any {
	out := map[string]any{}
	for k, v := range r {
		n, isInt := v.(int64)
		switch {
		case slices.Contains(timeColumns, k) && isInt:
			if n == 0 {
				out[k] = nil
			} else {
				out[k] = time.Unix(n, 0).UTC().Format(time.RFC3339)
			}
		case slices.Contains(boolColumns, k) && isInt:
			out[k] = n != 0
		default:
			out[k] = v
		}
	}
	return out
}

func renderAll(rows []store.Row) []map[string]any {
	out := []map[string]any{}
	for _, r := range rows {
		out = append(out, render(r))
	}
	return out
}

// appendDeny adds the deny-list row for a handle or key, every column of
// it, if there is one.
func appendDeny(tx *store.Tx, out []map[string]any, kind, value string) ([]map[string]any, error) {
	rows, err := tx.Rows("deny", "value", value)
	if err != nil {
		return nil, err
	}
	for _, r := range renderAll(rows) {
		if r["kind"] == kind {
			out = append(out, r)
		}
	}
	return out, nil
}

// Show is what admin show prints for a handle: its row, its deny entry, and
// its account's /v1/me without the email (which the operator can see at the
// merchant of record, and which is never fetched for this).
func Show(ctx context.Context, st *store.Store, handle string, now time.Time) (map[string]any, error) {
	var h store.Handle
	out := map[string]any{}
	err := st.View(ctx, func(tx *store.Tx) error {
		var err error
		if h, err = tx.Handle(handle); err != nil {
			return fmt.Errorf("no handle %q: %w", handle, err)
		}
		rows, err := tx.Rows("handles", "name", handle)
		if err != nil {
			return err
		}
		out["handle"] = renderAll(rows)[0]
		out["deny"] = nil
		deny, err := appendDeny(tx, nil, store.DenyHandle, handle)
		if len(deny) > 0 {
			out["deny"] = deny[0]
		}
		return err
	})
	if err != nil || h.Account == "" {
		return out, err
	}
	doc, _, err := accountDoc(ctx, st, h.Account, now)
	out["account"] = doc
	return out, err
}
