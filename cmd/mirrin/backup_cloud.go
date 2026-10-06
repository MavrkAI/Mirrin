package main

// Backups with Mirrin Cloud, the optional paid service, and moving backups
// between any two places. Cloud is only ever a place the owner names:
// `mirrin backup target cloud`, `mirrin backup init --cloud`, `mirrin
// restore --from cloud` and `mirrin backup migrate`. It is listed last,
// after the free places, wherever backups are described.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/cloud/backuptarget"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// The backup package can't see the paid service's client; this is how it
// opens that target, for this twin's link.
func init() { backup.OpenCloud = openCloudTarget }

func openCloudTarget(ns string) (backup.Target, error) {
	dataDir := filepath.Join(config.Home(), "data")
	if cfg, _ := loadForBackup(); cfg != nil && cfg.DataDir != "" {
		dataDir = cfg.DataDir
	}
	c, err := backupCloudClient(dataDir)
	if err != nil {
		return nil, err
	}
	return backuptarget.NewFor(c, ns), nil
}

// backupCloudClient is the client for this machine's link, checked with
// the keys this build trusts.
func backupCloudClient(dataDir string) (*cloud.Client, error) {
	st, err := cloud.OpenStateWithKeys(dataDir, cloudKeys())
	if err != nil {
		return nil, err
	}
	info, ok, err := st.Info()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("this machine isn't linked to Mirrin Cloud (mirrin cloud link), so backups can't go there")
	}
	return cloud.New(dataDir, info.API, cloudKeys())
}

// backupTargetCloud sends backups to the paid service: the words bind its
// namespace to this machine's account once, then this machine's own key
// signs every request.
func backupTargetCloud(ctx context.Context, t *terminal) error {
	cfg, err := loadForBackup()
	if err != nil {
		return err
	}
	c, err := backupCloudClient(cfg.DataDir)
	if err != nil {
		return err
	}
	s := cfg.Backup
	if s.Recipient == "" {
		return errors.New("backups aren't on yet; run `mirrin backup init --cloud` to turn them on and keep them there")
	}
	fmt.Fprintln(t.msg, "Mirrin Cloud needs your 12 words once, to know these backups are yours. They stay on this computer.")
	p, err := wordsFor(t, s)
	if err != nil {
		return err
	}
	if err := backuptarget.Bind(ctx, c, p); err != nil {
		return err
	}
	if _, err := backup.SetTarget(config.Path(), config.Backup{Target: backup.TargetCloud}); err != nil {
		return err
	}
	fmt.Fprintln(t.out, "Backups now go to Mirrin Cloud, encrypted with your words. The ones already made stay where they are; `mirrin backup migrate` copies them.")
	return nil
}

// bindForInit checks, before new words are made, that this machine is
// linked, and returns what binds the words once they are.
func bindForInit(ctx context.Context, dataDir string) (func(backup.Phrase) error, error) {
	c, err := backupCloudClient(dataDir)
	if err != nil {
		return nil, err
	}
	return func(p backup.Phrase) error { return backuptarget.Bind(ctx, c, p) }, nil
}

const migrateUsage = `usage: mirrin backup migrate [--from <place>] --to <place>
  A place is a folder path (or folder <path>), icloud, s3://<bucket>/<folder>, or cloud.
  --from defaults to where backups go now. Backups are copied as they are, encrypted, byte for byte.`

// takePlace reads a place at args[i]: "folder <path>", "icloud",
// "s3://…", "cloud", or a folder path. It returns the index of its last
// argument.
func takePlace(args []string, i int, s3o *s3Options) (config.Backup, int, error) {
	a := args[i]
	switch {
	case a == "icloud":
		return config.Backup{Target: backup.TargetICloud}, i, nil
	case a == "cloud":
		return config.Backup{Target: backup.TargetCloud}, i, nil
	case strings.HasPrefix(a, "s3://"):
		w, err := s3o.where(a)
		return w, i, err
	case a == "folder":
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return config.Backup{}, i, errors.New("folder needs a path: --to folder /Volumes/Backup")
		}
		i++
		a = args[i]
	}
	p, err := filepath.Abs(expandTilde(a))
	return config.Backup{Target: backup.TargetFolder, Path: p}, i, err
}

func backupMigrate(ctx context.Context, t *terminal, args []string) error {
	var from, to *config.Backup
	var s3o s3Options
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--from", "--to":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a place\n%s", a, migrateUsage)
			}
			w, j, err := takePlace(args, i+1, &s3o)
			if err != nil {
				return err
			}
			if a == "--from" {
				from = &w
			} else {
				to = &w
			}
			i = j
		default:
			j, ok, err := s3o.take(args, i)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("unknown option %s\n%s", a, migrateUsage)
			}
			i = j
		}
	}
	if to == nil {
		return errors.New(migrateUsage)
	}
	cfg, err := loadForBackup()
	if err != nil {
		return err
	}
	s := cfg.Backup
	if s.Recipient == "" {
		return errors.New("backups aren't set up here yet; run `mirrin backup init` (or `mirrin restore` to bring a twin back)")
	}
	ns, err := backup.NamespaceOf(s.RecoveryPub)
	if err != nil {
		return err
	}
	if from == nil {
		from = &config.Backup{Target: s.Target, Path: s.Path, S3: s.S3}
	}
	open := func(w config.Backup) (backup.Target, error) {
		if err := s3Keys(t, w, false); err != nil {
			return nil, err
		}
		return backup.OpenTarget(w, ns)
	}
	src, err := open(*from)
	if err != nil {
		return err
	}
	dst, err := open(*to)
	if err != nil {
		return err
	}
	if err := backup.CheckWhere(config.Path(), cfg.DataDir, *to); err != nil {
		return err
	}
	if err := backup.Prepare(dst); err != nil {
		return err
	}
	fmt.Fprintf(t.msg, "Copying backups from %s to %s…\n", src, dst)
	m, err := backup.Migrate(ctx, src, dst, func(name string, size int64) {
		fmt.Fprintf(t.msg, "  %s  %s\n", name, sizeText(size))
	})
	if err != nil {
		if len(m.Copied) > 0 {
			return fmt.Errorf("%v (%s copied before that; run the same command again to carry on)", err, plural(len(m.Copied), "backup"))
		}
		return err
	}
	fmt.Fprintf(t.out, "Copied %s (%s) from %s to %s, byte for byte; each copy was read back and checked.", plural(len(m.Copied), "backup"), sizeText(m.Bytes), src, dst)
	switch n := len(m.Same); {
	case n == 1:
		fmt.Fprint(t.out, " 1 was there already.")
	case n > 1:
		fmt.Fprintf(t.out, " %d were there already.", n)
	}
	if n := len(m.NotKept); n > 0 {
		keeps := "only some backups"
		if k, ok := dst.(backup.Keeper); ok {
			keeps = k.Keeps()
		}
		which := fmt.Sprintf("%d older ones are", n)
		if n == 1 {
			which = "1 older one is"
		}
		fmt.Fprintf(t.out, " %s keeps %s, so %s not kept there.", dst, keeps, which)
	}
	fmt.Fprintln(t.out, " Nothing was removed from "+src.String()+".")
	if backup.Where(*to) != backup.Where(s) {
		fmt.Fprintf(t.out, "New backups still go to %s. To send them to %s from now on: mirrin backup target %s\n", backup.Where(s), backup.Where(*to), targetWords(*to))
	}
	return nil
}

// targetWords is how `mirrin backup target` names a place.
func targetWords(w config.Backup) string {
	switch w.Target {
	case backup.TargetFolder:
		return "folder " + w.Path
	case backup.TargetS3:
		u := "s3://" + w.S3.Bucket
		if w.S3.Prefix != "" {
			u += "/" + w.S3.Prefix
		}
		return "s3 " + u
	case "":
		return backup.TargetICloud
	}
	return w.Target
}

// cloudRestore is a restore from the paid service, made with the words on
// a machine that may be new: the account moves here first (the machine
// that had it stands by), into a link folder beside the home that moves in
// with the twin once it is restored.
type cloudRestore struct {
	dir    string // the link's data folder until the restore
	target backup.Target
}

// cloudRestoreDir is where a recovery keeps its link until the twin is in
// place, beside the home so it survives a failed restore and is picked up
// again by the next try.
func cloudRestoreDir(home string) string {
	return filepath.Join(filepath.Dir(home), "."+filepath.Base(home)+".cloud-restore")
}

// recoverForRestore moves the account here, unless an earlier try did.
func recoverForRestore(ctx context.Context, t *terminal, f restoreFlags, p backup.Phrase) (*cloudRestore, error) {
	keys := cloudKeys()
	if len(keys) == 0 {
		return nil, errors.New("this build trusts no Mirrin Cloud signing key, so it can't restore from there. " +
			"Mirrin Cloud has not launched yet; development builds add the dev keys with -tags mirrin_devkeys")
	}
	home := config.Home()
	r := &cloudRestore{dir: cloudRestoreDir(home)}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, err
	}
	api := f.api
	if api == "" {
		if st, err := cloud.OpenStateWithKeys(r.dir, keys); err == nil {
			if info, ok, _ := st.Info(); ok {
				api = info.API
			}
		}
	}
	if api == "" {
		if cfg, _ := cloudConfig(); cfg != nil {
			api = cfg.Cloud.API
		}
	}
	if api == "" {
		api = cloud.DefaultAPI
	}
	c, err := cloud.New(r.dir, api, keys)
	if err != nil {
		return nil, err
	}
	if !c.State().Linked() {
		if !f.yes {
			if !t.tty {
				return nil, errors.New("restoring from Mirrin Cloud makes this computer the twin's home, and the machine that has it now stands by; add --yes to go ahead")
			}
			a, err := t.ask("Restoring from Mirrin Cloud makes this computer the twin's home: the machine that has it now stands by, and a twin here is kept aside. Go ahead? [y/N] ")
			if err != nil || !strings.HasPrefix(strings.ToLower(a), "y") {
				return nil, errors.New("nothing was changed")
			}
		}
		rec, err := backuptarget.Recover(ctx, c, p, "")
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(t.msg, "Mirrin Cloud moved %s to this computer.\n", rec.Handle)
		if !rec.Entitled {
			fmt.Fprintln(t.msg, "Payment for it has lapsed: the backups there can be restored for 90 days, but nothing new is kept.")
		}
	}
	r.target = backuptarget.NewFor(c, p.Namespace())
	return r, nil
}

// settle moves the link into the restored twin's data folder.
func (r *cloudRestore) settle(newData string) error {
	dst := filepath.Join(newData, "cloud")
	if _, err := os.Stat(dst); err == nil {
		aside := dst + ".before-restore-" + time.Now().UTC().Format("20060102-150405")
		if err := os.Rename(dst, aside); err != nil {
			return err
		}
	}
	if err := os.Rename(filepath.Join(r.dir, "cloud"), dst); err != nil {
		return err
	}
	return os.RemoveAll(r.dir)
}
