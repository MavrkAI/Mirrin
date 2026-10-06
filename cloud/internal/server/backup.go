package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/storage"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// Backup storage (docs/cloud-api.md §5.7). The daemon's backups go to
// storage directly, at presigned URLs good for 15 minutes; the control plane
// only decides what may be stored and counts what was. It sees object names
// (a time and eight random hex digits) and sizes, never a byte of content,
// which is ciphertext only the owner's 12 words open.
const (
	presignTTL = 15 * time.Minute
	// readGrace is how long after paid_through a lapsed account can still
	// list and fetch its backups (docs/cloud-design.md §10: Expired).
	readGrace = 90 * 24 * time.Hour
	// maxObject bounds one object, whatever the quota.
	maxObject = 64 << 30
	// uploadGrace is how long after its URL runs out an upload that was
	// never committed still holds its size, since a PUT that started in
	// time may still be arriving. Then the sweep deletes its key.
	uploadGrace = 6 * time.Hour
)

func (s *Server) backupRoutes(handle func(method, path string, h http.HandlerFunc)) {
	handle("POST", "/v1/backup/namespaces", s.signed(deviceKeys, s.holder(s.bindNamespace)))
	handle("POST", "/v1/backup/presign", s.signed(deviceKeys, s.holder(s.presign)))
	handle("GET", "/v1/backup/objects", s.signed(deviceKeys, s.holder(s.listObjects)))
	handle("POST", "/v1/backup/commit", s.signed(deviceKeys, s.holder(s.commit)))
	handle("POST", "/v1/recover", s.limitPreAuth(s.signed(recoverKeys, s.recover)))
}

// offersBackup answers 404 when this server keeps no backups, and 403 when
// its plan doesn't include them.
func (s *Server) offersBackup(w http.ResponseWriter) bool {
	if s.storage == nil {
		fail(w, http.StatusNotFound, "not_found", "This server keeps no backups.")
		return false
	}
	if !slices.Contains(s.cfg.Features, "backup") {
		fail(w, http.StatusForbidden, "not_included", "Backups aren't part of this plan.")
		return false
	}
	return true
}

// backupState is what a backup route needs to know about the account.
type backupState struct {
	ns       store.Namespace
	held     int64 // pending uploads and objects not yet gone from storage
	bound    bool
	lapsed   bool // no new objects: payment has lapsed
	readable bool // objects can still be listed and fetched
}

func (s *Server) backupState(tx *store.Tx, account string, now time.Time) (backupState, error) {
	a, err := tx.Account(account)
	if err != nil {
		return backupState{}, err
	}
	b := backupState{lapsed: !now.Before(a.PaidThrough), readable: now.Before(a.PaidThrough.Add(readGrace))}
	b.ns, err = tx.AccountNamespace(account)
	switch {
	case err == nil:
		b.bound = true
		b.held, err = tx.Held(b.ns.NS)
		return b, err
	case !errors.Is(err, store.ErrNotFound):
		return b, err
	}
	return b, nil
}

// bindNamespace answers POST /v1/backup/namespaces {ns, recovery_pub,
// sig_recovery}: the namespace is bound to this device's account, which the
// recovery key's signature over the device key allows. Binding it again
// from the same account is a no-op; an account whose backups made with
// other words are still there can't bind new ones.
func (s *Server) bindNamespace(w http.ResponseWriter, r *http.Request, d store.Device, body []byte) {
	if !s.offersBackup(w) {
		return
	}
	var in struct {
		NS          string `json:"ns"`
		RecoveryPub string `json:"recovery_pub"`
		Sig         string `json:"sig_recovery"`
	}
	if decodeStrict(body, &in) != nil {
		fail(w, http.StatusBadRequest, "bad_request", "The request body is not what backup/namespaces takes.")
		return
	}
	pub, err := entitle.ParseKey(in.RecoveryPub)
	if err != nil || !storage.ValidNamespace(in.NS) || entitle.Namespace(pub) != in.NS {
		fail(w, http.StatusBadRequest, "bad_request", "ns must be the namespace of recovery_pub.")
		return
	}
	if !verifyRecovery(pub, in.Sig, entitle.BindMessage(in.NS, in.RecoveryPub, d.Pub)) {
		fail(w, http.StatusUnauthorized, "unauthorized", "The recovery signature was not accepted.")
		return
	}
	now := s.now().UTC().Truncate(time.Second)
	var code string
	err = s.store.Update(r.Context(), func(tx *store.Tx) error {
		if n, err := tx.Namespace(in.NS); err == nil {
			if n.Account != d.Account {
				code = "namespace_taken"
			}
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if cur, err := tx.AccountNamespace(d.Account); err == nil {
			objs, err := tx.Objects(cur.NS)
			if err != nil {
				return err
			}
			held, err := tx.Held(cur.NS)
			if err != nil {
				return err
			}
			if len(objs) > 0 || held > 0 {
				code = "namespace_bound"
				return nil
			}
			if err := tx.DeleteNamespace(cur.NS); err != nil {
				return err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.InsertNamespace(store.Namespace{NS: in.NS, RecoveryPub: in.RecoveryPub, Account: d.Account, Created: now}); err != nil {
			return err
		}
		return tx.AddEvent(d.Account, "backup_bound", now)
	})
	switch {
	case err != nil:
		internal(w, s, err)
	case code == "namespace_taken":
		fail(w, http.StatusConflict, "namespace_taken", "These backup words are in use by another account.")
	case code == "namespace_bound":
		fail(w, http.StatusConflict, "namespace_bound", "This account keeps backups made with other words. Delete those first to keep backups made with these words here.")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// verifyRecovery checks a recovery signature (unpadded base64url) over msg.
func verifyRecovery(pub ed25519.PublicKey, sig string, msg []byte) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	return err == nil && len(b) == ed25519.SignatureSize && ed25519.Verify(pub, msg, b)
}

// presign answers POST /v1/backup/presign {op, name, size}: a URL for one
// put, get or delete of one object of this account's namespace, at
// ns/<ns>/<name> and no other key. A put is refused for a name stored or
// removed before (409), over the quota (413) or once payment has lapsed
// (402); a get once the 90 days after that have passed (402).
//
// The quota is enforced here: a put's size is held from its presign until
// it is committed, deleted, or swept once its URL is long gone, so what is
// uploaded and never committed still counts. A delete is done by the
// control plane itself before it answers; the URL it hands back finds
// nothing left to delete.
func (s *Server) presign(w http.ResponseWriter, r *http.Request, d store.Device, body []byte) {
	if !s.offersBackup(w) {
		return
	}
	var in struct {
		Op   string `json:"op"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if decodeStrict(body, &in) != nil || !storage.ValidName(in.Name) || in.Size < 0 ||
		!slices.Contains([]string{storage.OpPut, storage.OpGet, storage.OpDelete}, in.Op) {
		fail(w, http.StatusBadRequest, "bad_request", "presign takes an op (put, get or delete), a backup object's name and a size.")
		return
	}
	if in.Op != storage.OpPut {
		in.Size = 0
	}
	if in.Op == storage.OpDelete {
		s.deleteObject(w, r, d, in.Name)
		return
	}
	now := s.now().UTC()
	var st backupState
	var code string
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		var err error
		if st, err = s.backupState(tx, d.Account, now); err != nil || !st.bound {
			return err
		}
		_, err = tx.Object(st.ns.NS, in.Name)
		exists := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if in.Op == storage.OpGet {
			switch {
			case !st.readable:
				code = "lapsed"
			case !exists:
				code = "not_found"
			}
			return nil
		}
		gone, err := tx.HasTombstone(st.ns.NS, in.Name)
		if err != nil {
			return err
		}
		up, err := tx.Upload(st.ns.NS, in.Name)
		pending := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		held := st.held
		if pending {
			held -= up.Size // presigned again: the same key, held once
		}
		switch {
		case st.lapsed:
			code = "lapsed"
		case gone:
			code = "removed"
		case exists || pending && up.Size != in.Size:
			code = "exists"
		case in.Size > maxObject || st.ns.Bytes+held+in.Size > s.cfg.BackupQuota:
			code = "over_quota"
		default:
			return tx.PutUpload(store.Upload{NS: st.ns.NS, Name: in.Name, Size: in.Size, Expires: now.Add(presignTTL)})
		}
		return nil
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	if !s.backupRefusal(w, st, code) {
		return
	}
	key, err := storage.Key(st.ns.NS, in.Name)
	if err != nil {
		internal(w, s, err)
		return
	}
	signed, err := s.storage.Presign(r.Context(), in.Op, key, in.Size, presignTTL)
	if err != nil {
		internal(w, s, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"url": signed.URL, "method": signed.Method, "headers": signed.Headers,
		"expires": signed.Expires.UTC().Format(time.RFC3339)})
}

// backupRefusal answers a refused backup request and reports false, or
// reports true when there is nothing to refuse.
func (s *Server) backupRefusal(w http.ResponseWriter, st backupState, code string) bool {
	switch {
	case !st.bound:
		fail(w, http.StatusForbidden, "no_namespace", "Backups aren't set up for this account yet: bind a namespace with your backup words first.")
	case code == "lapsed" && st.readable:
		fail(w, http.StatusPaymentRequired, "lapsed", "Payment has lapsed, so no new backups are kept. The ones here can be fetched for 90 days.")
	case code == "lapsed":
		fail(w, http.StatusPaymentRequired, "lapsed", "Payment lapsed more than 90 days ago, so these backups can no longer be fetched.")
	case code == "exists":
		fail(w, http.StatusConflict, "exists", "An object of that name is stored already; names are never reused.")
	case code == "removed":
		fail(w, http.StatusConflict, "removed", "An object of that name was removed; names are never reused.")
	case code == "over_quota":
		reply(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "over_quota", "message": "That would go over this account's backup space.",
			"quota": s.cfg.BackupQuota, "bytes": st.ns.Bytes, "held": st.held})
	case code == "not_found":
		fail(w, http.StatusNotFound, "not_found", "No backup of that name is stored.")
	case code == "size_mismatch":
		fail(w, http.StatusConflict, "size_mismatch", "What arrived isn't the size that was allowed, so it was removed.")
	case code == "not_uploaded":
		fail(w, http.StatusConflict, "not_uploaded", "Nothing arrived under that name.")
	default:
		return true
	}
	return false
}

// listObjects answers GET /v1/backup/objects: every stored object's name,
// size and when it was stored, oldest name first, and the space used.
func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, d store.Device, _ []byte) {
	if !s.offersBackup(w) {
		return
	}
	now := s.now().UTC()
	var st backupState
	var objs []store.StoredObject
	err := s.store.View(r.Context(), func(tx *store.Tx) error {
		var err error
		if st, err = s.backupState(tx, d.Account, now); err != nil || !st.bound {
			return err
		}
		objs, err = tx.Objects(st.ns.NS)
		return err
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	code := ""
	if !st.readable {
		code = "lapsed"
	}
	if !s.backupRefusal(w, st, code) {
		return
	}
	list := []map[string]any{}
	for _, o := range objs {
		list = append(list, map[string]any{"name": o.Name, "size": o.Size, "created": o.Created.UTC().Format(time.RFC3339)})
	}
	reply(w, http.StatusOK, map[string]any{"ns": st.ns.NS, "objects": list, "bytes": st.ns.Bytes, "held": st.held, "quota": s.cfg.BackupQuota})
}

// commit answers POST /v1/backup/commit {name, size}: a presigned upload
// is counted as stored once a HEAD finds exactly size bytes under its key.
// Something of another size is removed, and its name is not used again.
// Then retention runs: 7 daily, 4 weekly and 6 monthly snapshots, and never
// the newest.
func (s *Server) commit(w http.ResponseWriter, r *http.Request, d store.Device, body []byte) {
	if !s.offersBackup(w) {
		return
	}
	var in struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if decodeStrict(body, &in) != nil || !storage.ValidName(in.Name) || in.Size < 0 {
		fail(w, http.StatusBadRequest, "bad_request", "commit takes a backup object's name and its size.")
		return
	}
	now := s.now().UTC().Truncate(time.Second)
	var st backupState
	var up store.Upload
	var code string
	err := s.store.View(r.Context(), func(tx *store.Tx) error {
		var err error
		if st, err = s.backupState(tx, d.Account, now); err != nil || !st.bound {
			return err
		}
		if o, err := tx.Object(st.ns.NS, in.Name); err == nil {
			if o.Size != in.Size {
				code = "exists"
			} else {
				code = "done"
			}
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		up, err = tx.Upload(st.ns.NS, in.Name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			gone, err := tx.HasTombstone(st.ns.NS, in.Name)
			if err != nil {
				return err
			}
			code = "not_uploaded"
			if gone {
				code = "removed"
			}
			return nil
		case err != nil:
			return err
		case up.Size != in.Size:
			code = "size_mismatch"
		case st.lapsed:
			code = "lapsed"
		}
		return nil
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	if code == "done" {
		reply(w, http.StatusOK, map[string]any{"bytes": st.ns.Bytes, "pruned": []string{}})
		return
	}
	if code == "size_mismatch" {
		s.discardUpload(r, up, in.Size, now)
	}
	if !s.backupRefusal(w, st, code) {
		return
	}
	key, _ := storage.Key(st.ns.NS, in.Name)
	size, err := s.storage.Head(r.Context(), key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		s.backupRefusal(w, st, "not_uploaded")
		return
	case err != nil:
		internal(w, s, err)
		return
	case size != in.Size:
		s.discardUpload(r, up, size, now)
		s.backupRefusal(w, st, "size_mismatch")
		return
	}
	var pruned []store.StoredObject
	err = s.store.Update(r.Context(), func(tx *store.Tx) error {
		var err error
		if st, err = s.backupState(tx, d.Account, now); err != nil || !st.bound {
			return err
		}
		if _, err := tx.Object(st.ns.NS, in.Name); err == nil {
			code = "exists"
			return nil
		}
		// The sweep may have taken the upload meanwhile.
		if cur, err := tx.Upload(st.ns.NS, in.Name); errors.Is(err, store.ErrNotFound) || err == nil && cur.Size != in.Size {
			code = "not_uploaded"
			return nil
		} else if err != nil {
			return err
		}
		if st.ns.Bytes+in.Size > s.cfg.BackupQuota {
			code = "over_quota"
			return nil
		}
		if _, err := tx.DeleteUpload(st.ns.NS, in.Name); err != nil {
			return err
		}
		if err := tx.InsertObject(store.StoredObject{NS: st.ns.NS, Name: in.Name, Size: in.Size, Created: now}); err != nil {
			return err
		}
		objs, err := tx.Objects(st.ns.NS)
		if err != nil {
			return err
		}
		for _, name := range retentionDrops(objs, in.Name, now) {
			o, err := tx.Object(st.ns.NS, name)
			if err != nil {
				return err
			}
			if _, err := tx.DeleteObject(st.ns.NS, name); err != nil {
				return err
			}
			// Held until storage is rid of it, and deleted again once any
			// URL handed out for it has run out.
			if err := tx.AddTombstone(store.Tombstone{NS: st.ns.NS, Name: name, Size: o.Size, Due: now.Add(presignTTL), Created: now}); err != nil {
				return err
			}
			pruned = append(pruned, o)
		}
		st.ns, err = tx.Namespace(st.ns.NS)
		return err
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	if !s.backupRefusal(w, st, code) {
		return
	}
	names := []string{}
	for _, o := range pruned {
		names = append(names, o.Name)
		k, _ := storage.Key(st.ns.NS, o.Name)
		if err := s.storage.DeletePrefix(r.Context(), k); err != nil {
			s.log.Warn("backup: prune", "err", err)
			continue // the sweep deletes it when it falls due
		}
		s.releaseTombstone(r.Context(), st.ns.NS, o.Name, false)
	}
	reply(w, http.StatusOK, map[string]any{"bytes": st.ns.Bytes, "pruned": names})
}

// discardUpload gives up on an upload that isn't what was allowed: its name
// is not used again, and its key is deleted now and once more when a PUT
// its URL let start can no longer be arriving. Until then it holds the
// larger of the size allowed and the size found.
func (s *Server) discardUpload(r *http.Request, up store.Upload, found int64, now time.Time) {
	err := s.store.Update(r.Context(), func(tx *store.Tx) error {
		if _, err := tx.DeleteUpload(up.NS, up.Name); err != nil {
			return err
		}
		return tx.AddTombstone(store.Tombstone{NS: up.NS, Name: up.Name, Size: max(up.Size, found), Due: up.Expires.Add(uploadGrace), Created: now})
	})
	if err != nil {
		s.log.Warn("backup: discard an upload", "err", err)
		return
	}
	key, _ := storage.Key(up.NS, up.Name)
	if err := s.storage.DeletePrefix(r.Context(), key); err != nil {
		s.log.Warn("backup: remove a wrong-sized object", "err", err)
	}
}

// deleteObject answers a presign for a delete. The control plane deletes
// the object itself, holding the storage credentials, so nothing a daemon
// does or fails to do afterwards leaves bytes stored and uncounted. The
// name is not used again. A committed object stops counting once storage
// is rid of it; an upload never committed holds its size until a PUT its
// URL let start can no longer be arriving, when its key is deleted again.
// The answer is a presigned DELETE, as for any op, which finds nothing.
func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, d store.Device, name string) {
	now := s.now().UTC().Truncate(time.Second)
	var st backupState
	err := s.store.View(r.Context(), func(tx *store.Tx) error {
		var err error
		st, err = s.backupState(tx, d.Account, now)
		return err
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	if !s.backupRefusal(w, st, "") {
		return
	}
	key, err := storage.Key(st.ns.NS, name)
	if err != nil {
		internal(w, s, err)
		return
	}
	if err := s.storage.DeletePrefix(r.Context(), key); err != nil {
		internal(w, s, err)
		return
	}
	err = s.store.Update(r.Context(), func(tx *store.Tx) error {
		if st, err = s.backupState(tx, d.Account, now); err != nil || !st.bound {
			return err
		}
		committed, err := tx.DeleteObject(st.ns.NS, name)
		if err != nil {
			return err
		}
		up, err := tx.Upload(st.ns.NS, name)
		switch {
		case err == nil:
			if _, err := tx.DeleteUpload(st.ns.NS, name); err != nil {
				return err
			}
			return tx.AddTombstone(store.Tombstone{NS: st.ns.NS, Name: name, Size: up.Size, Due: up.Expires.Add(uploadGrace), Created: now})
		case !errors.Is(err, store.ErrNotFound):
			return err
		case committed:
			return tx.AddTombstone(store.Tombstone{NS: st.ns.NS, Name: name, Due: now.Add(presignTTL), Created: now})
		}
		return nil
	})
	if err != nil {
		internal(w, s, err)
		return
	}
	if !s.backupRefusal(w, st, "") {
		return
	}
	signed, err := s.storage.Presign(r.Context(), storage.OpDelete, key, 0, presignTTL)
	if err != nil {
		internal(w, s, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"url": signed.URL, "method": signed.Method, "headers": signed.Headers,
		"expires": signed.Expires.UTC().Format(time.RFC3339)})
}

// releaseTombstone stops holding a removed object's size once storage is
// rid of it; with done, its key needs no deleting again either.
func (s *Server) releaseTombstone(ctx context.Context, ns, name string, done bool) {
	if err := s.store.Update(ctx, func(tx *store.Tx) error { return tx.ReleaseTombstone(ns, name, done) }); err != nil {
		s.log.Warn("backup: release a removed name", "err", err)
	}
}

// sweepBackups does the backups' timed work: an upload never committed is
// given up a while after its URL ran out, and the key of every removed
// name that falls due is deleted, after which it stops counting against
// the quota.
func (s *Server) sweepBackups(ctx context.Context, now time.Time) error {
	if s.storage == nil {
		return nil
	}
	var due []store.Tombstone
	err := s.store.Update(ctx, func(tx *store.Tx) error {
		ups, err := tx.UploadsExpiredBy(now.Add(-uploadGrace))
		if err != nil {
			return err
		}
		for _, u := range ups {
			if _, err := tx.DeleteUpload(u.NS, u.Name); err != nil {
				return err
			}
			if err := tx.AddTombstone(store.Tombstone{NS: u.NS, Name: u.Name, Size: u.Size, Due: now, Created: now}); err != nil {
				return err
			}
		}
		due, err = tx.TombstonesDueBy(now)
		return err
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, tb := range due {
		key, err := storage.Key(tb.NS, tb.Name)
		if err == nil {
			err = s.storage.DeletePrefix(ctx, key)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("deleting a removed backup: %w", err))
			continue
		}
		s.releaseTombstone(ctx, tb.NS, tb.Name, true)
	}
	return errors.Join(errs...)
}
