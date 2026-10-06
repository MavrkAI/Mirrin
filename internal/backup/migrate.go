package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Migrated is what Migrate did.
type Migrated struct {
	// Copied are the objects copied, oldest first; Same those the
	// destination held already, byte for byte. NotKept are those the
	// destination doesn't keep (Keeper): copied and since dropped by its
	// retention, or dropped by it before and not taken again.
	Copied, Same, NotKept []string
	// Bytes is how much was copied.
	Bytes int64
}

// Migrate copies every snapshot and handover marker from one target to
// another, byte for byte: the ciphertext moves as it is, so no words are
// needed and nothing is decrypted. Each copy is read back from the
// destination and its SHA-256 compared with what left the source. An
// object the destination has already is left alone when its bytes are the
// same, and is an error when they differ, since a name is never replaced.
// The source keeps everything. progress, if not nil, hears each name before
// it is copied.
func Migrate(ctx context.Context, from, to Target, progress func(name string, size int64)) (Migrated, error) {
	var m Migrated
	if sameTarget(from, to) {
		return m, fmt.Errorf("%s is where the backups are already", to)
	}
	objs, err := from.List(ctx)
	if err != nil {
		return m, fmt.Errorf("couldn't list the backups in %s (%v)", from, err)
	}
	there, err := to.List(ctx)
	if err != nil {
		return m, fmt.Errorf("couldn't list what %s holds (%v)", to, err)
	}
	have := map[string]int64{}
	for _, o := range there {
		have[o.Name] = o.Size
	}
	for _, o := range objs {
		if err := ctx.Err(); err != nil {
			return m, err
		}
		if !ValidName(o.Name) {
			continue
		}
		sum, n, err := hashObject(ctx, from, o.Name)
		if err != nil {
			return m, fmt.Errorf("couldn't read %s from %s (%v)", o.Name, from, err)
		}
		if size, ok := have[o.Name]; ok {
			theirs, tn, err := hashObject(ctx, to, o.Name)
			if err != nil {
				return m, fmt.Errorf("couldn't read %s from %s (%v)", o.Name, to, err)
			}
			if (size != 0 && size != n) || tn != n || theirs != sum { // 0: size unknown (an evicted iCloud file)
				return m, fmt.Errorf("%s holds a different %s; nothing of it was replaced", to, o.Name)
			}
			m.Same = append(m.Same, o.Name)
			continue
		}
		if progress != nil {
			progress(o.Name, n)
		}
		if err := copyObject(ctx, from, to, o.Name, n, sum); errors.Is(err, ErrNotKept) {
			m.NotKept = append(m.NotKept, o.Name)
			continue
		} else if err != nil {
			return m, err
		}
		m.Copied = append(m.Copied, o.Name)
		m.Bytes += n
	}
	if _, ok := to.(Keeper); ok && len(m.Copied) > 0 {
		// Its retention may have dropped some of what was copied since.
		there, err := to.List(ctx)
		if err != nil {
			return m, fmt.Errorf("couldn't list what %s holds (%v)", to, err)
		}
		kept := map[string]bool{}
		for _, o := range there {
			kept[o.Name] = true
		}
		var copied []string
		for _, name := range m.Copied {
			if kept[name] {
				copied = append(copied, name)
			} else {
				m.NotKept = append(m.NotKept, name)
			}
		}
		m.Copied = copied
	}
	return m, nil
}

// copyObject streams name from one target to the other and reads it back.
func copyObject(ctx context.Context, from, to Target, name string, size int64, sum string) error {
	rc, err := from.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("couldn't read %s from %s (%v)", name, from, err)
	}
	defer rc.Close()
	h := sha256.New()
	if err := to.Put(ctx, name, io.TeeReader(rc, h), size); err != nil {
		if errors.Is(err, ErrNotKept) {
			return err
		}
		if errors.Is(err, ErrWritesClosed) {
			return fmt.Errorf("couldn't copy %s to %s: %v", name, to, err)
		}
		return fmt.Errorf("couldn't copy %s to %s (%v)", name, to, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		_ = to.Delete(ctx, name)
		return fmt.Errorf("%s changed in %s while it was copied; run the migration again", name, from)
	}
	if err := readBack(ctx, to, name, size, sum); err != nil {
		_ = to.Delete(ctx, name)
		return fmt.Errorf("the copy of %s in %s didn't read back intact (%v)", name, to, err)
	}
	return nil
}

// hashObject reads a whole object and returns its SHA-256 and size.
func hashObject(ctx context.Context, t Target, name string) (string, int64, error) {
	rc, err := t.Get(ctx, name)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
