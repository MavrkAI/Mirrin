package server

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/store"
)

// meSection is where /v1/me shows an account-linked table: the accounts
// row as the "account" object, every other table as a list under its name.
func meSection(table string) string {
	if table == "accounts" {
		return "account"
	}
	return table
}

// shownInMe reports whether /v1/me must show every column of a table: the
// account-linked tables, the deny list, whose rows for the account's keys
// and handle it shows, and the backup objects of its namespaces.
func shownInMe(tb store.Table) bool {
	return store.AccountLinked(tb) || tb.Name == "deny" || tb.Name == "objects"
}

// coverageGaps lists every column of a table /v1/me shows that it does not
// show. Each table must have rows in doc for its columns to be checked, so
// an empty section is a gap too.
func coverageGaps(tables []store.Table, doc map[string]any) []string {
	var gaps []string
	for _, tb := range tables {
		if !shownInMe(tb) {
			continue
		}
		var objs []map[string]any
		switch v := doc[meSection(tb.Name)].(type) {
		case map[string]any:
			objs = []map[string]any{v}
		case []any:
			for _, o := range v {
				if m, ok := o.(map[string]any); ok {
					objs = append(objs, m)
				}
			}
		}
		if len(objs) == 0 {
			gaps = append(gaps, fmt.Sprintf("%s: /v1/me has no %q rows", tb.Name, meSection(tb.Name)))
			continue
		}
		for _, c := range tb.Columns {
			for _, o := range objs {
				if _, ok := o[c]; !ok {
					gaps = append(gaps, fmt.Sprintf("%s.%s is missing from /v1/me", tb.Name, c))
					break
				}
			}
		}
	}
	return gaps
}

// /v1/me shows every column of every table that names the account. The
// schema is read from the live database, so a table or column added later
// is checked without anyone remembering to.
func TestMeShowsEveryStoredColumn(t *testing.T) {
	e := newEnv(t, false, nil)
	key := newKey(t)
	e.link(t, key, "velvet-yak-88", acme)
	send(t, e.signed(t, key, "PUT", "/v1/acme-account", []byte(`{"uri":"`+acme+`"}`)))
	// Backups: a namespace and an object in it.
	e.bind(t, key, newKey(t))
	e.upload(t, key, snapName(1), []byte("ciphertext"))
	send(t, e.signed(t, key, "DELETE", "/v1/account", nil))
	// A deny-list row about the account: its handle, suspended.
	if err := DenyHandle(t.Context(), e.store, "velvet-yak-88", "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	r := send(t, e.signed(t, key, "GET", "/v1/me", nil))
	if r.status != http.StatusOK {
		t.Fatalf("GET /v1/me: %d %s", r.status, r.body)
	}
	var doc map[string]any
	r.json(t, &doc)

	var tables []store.Table
	e.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		tables, err = tx.Schema()
		return err
	})
	linked := 0
	for _, tb := range tables {
		if shownInMe(tb) {
			linked++
		}
	}
	if linked < 8 {
		t.Fatalf("only %d tables to show found; the schema read is broken", linked)
	}
	for _, g := range coverageGaps(tables, doc) {
		t.Error(g)
	}
	if doc["email"] != "owner+"+doc["account"].(map[string]any)["billing_customer"].(string)+"@example.invalid" {
		t.Errorf("email %v, want the one fetched from the merchant of record", doc["email"])
	}
	if a := doc["account"].(map[string]any); a["status"] != "deleting" || a["billing_status"] != "active" || doc["deletion"] == nil {
		t.Errorf("account %v deletion %v", doc["account"], doc["deletion"])
	}

	// The check has teeth: drop one column, or one table, and it says so.
	acct := doc["account"].(map[string]any)
	delete(acct, "acme_account")
	if g := coverageGaps(tables, doc); !slices.ContainsFunc(g, func(s string) bool { return strings.Contains(s, "accounts.acme_account") }) {
		t.Errorf("a missing column was not caught: %v", g)
	}
	delete(doc, "events")
	if g := coverageGaps(tables, doc); !slices.ContainsFunc(g, func(s string) bool { return strings.HasPrefix(s, "events:") }) {
		t.Errorf("a missing table was not caught: %v", g)
	}
	delete(doc["deny"].([]any)[0].(map[string]any), "key_id")
	if g := coverageGaps(tables, doc); !slices.ContainsFunc(g, func(s string) bool { return strings.Contains(s, "deny.key_id") }) {
		t.Errorf("a missing deny-list column was not caught: %v", g)
	}
}

// Review test: the schema has no column for an email or an IP address, and
// after a full journey neither the email the merchant of record holds nor
// the client's address is anywhere in the database.
func TestNoEmailOrAddressStored(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)(mail|^ips?$|^ip_|_ips?$|addr|remote|forwarded|agent|phone|card)`)
	e := newEnv(t, false, nil)
	key := newKey(t)
	e.link(t, key, "", acme)
	r := send(t, e.signed(t, key, "GET", "/v1/me", nil))
	var doc map[string]any
	r.json(t, &doc)
	email, _ := doc["email"].(string)
	if !strings.Contains(email, "@") {
		t.Fatalf("no email came back from the merchant of record: %v", doc["email"])
	}
	send(t, e.signed(t, key, "POST", "/v1/billing/portal", nil))
	send(t, e.signed(t, key, "POST", "/v1/entitlement/refresh", nil))

	err := e.store.View(t.Context(), func(tx *store.Tx) error {
		tables, err := tx.Schema()
		if err != nil {
			return err
		}
		for _, tb := range tables {
			for _, c := range tb.Columns {
				if forbidden.MatchString(c) {
					t.Errorf("column %s.%s looks like it holds an email or an address", tb.Name, c)
				}
			}
		}
		dump, err := tx.Dump()
		if err != nil {
			return err
		}
		for table, rows := range dump {
			for _, row := range rows {
				for c, v := range row {
					s := fmt.Sprint(v)
					if strings.Contains(s, "@") || strings.Contains(s, "127.0.0.1") || strings.Contains(s, "::1") {
						t.Errorf("%s.%s holds %q", table, c, s)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
