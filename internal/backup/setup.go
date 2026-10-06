package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Setup keeps the public halves of p's keys in the config file at
// cfgPath, with where backups go, and notes in dataDir when backups began.
// The words themselves are not written anywhere.
func Setup(cfgPath, dataDir string, p Phrase, where config.Backup, now time.Time) (config.Backup, error) {
	s := where
	s.Recipient = p.Recipient()
	s.RecoveryPub = p.RecoveryPub()
	if s.Recipient == "" {
		return s, errors.New("couldn't make the backup key")
	}
	if err := outsideTwin(s, filepath.Dir(cfgPath), dataDir); err != nil {
		return s, err
	}
	t, err := OpenTarget(s, p.Namespace())
	if err != nil {
		return s, err
	}
	if err := makeFolder(t); err != nil {
		return s, err
	}
	if err := SaveSettings(cfgPath, s); err != nil {
		return s, err
	}
	err = UpdateState(dataDir, func(st *State) {
		st.Since, st.KitShown, st.Nudged = now, now, time.Time{}
		st.LastError = ""
		clearLast(st) // gone.go: new words, new folder
	})
	return s, err
}

// SetTarget changes where backups go and keeps the keys.
func SetTarget(cfgPath string, where config.Backup) (config.Backup, error) {
	s, err := LoadSettings(cfgPath)
	if err != nil {
		return s, err
	}
	s.Target, s.Path, s.S3 = where.Target, where.Path, where.S3
	if err := outsideTwin(s, filepath.Dir(cfgPath), dataDirOf(cfgPath)); err != nil {
		return s, err
	}
	if s.RecoveryPub != "" {
		ns, err := NamespaceOf(s.RecoveryPub)
		if err != nil {
			return s, err
		}
		t, err := OpenTarget(s, ns)
		if err != nil {
			return s, err
		}
		if err := makeFolder(t); err != nil {
			return s, err
		}
	}
	if err := SaveSettings(cfgPath, s); err != nil {
		return s, err
	}
	return s, forgetLast(dataDirOf(cfgPath)) // gone.go
}

// CheckWhere says whether backups can go where s says, for the twin whose
// config is at cfgPath, before any words are made: not inside the twin's
// own folders.
func CheckWhere(cfgPath, dataDir string, s config.Backup) error {
	return outsideTwin(s, filepath.Dir(cfgPath), dataDir)
}

// Prepare readies a target to be written to for the first time: a folder
// target's folder is made, and a bucket is checked.
func Prepare(t Target) error { return makeFolder(t) }

// makeFolder makes a folder target's folder, the folder the owner chose
// included: this is the one time it is made. A nightly backup never makes
// it, so a disk that isn't connected fails instead of filling a new folder.
func makeFolder(t Target) error {
	if s, ok := t.(*s3Target); ok {
		return checkS3(s)
	}
	if f, ok := t.(*folder); ok {
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			return fmt.Errorf("can't make the backup folder %s (%v)", f.dir, err)
		}
	}
	return nil
}

// outsideTwin refuses a backup folder inside the twin's own folders: it
// would sit on the disk it is meant to protect, and a restore would move
// it aside with the twin.
func outsideTwin(s config.Backup, home, dataDir string) error {
	if s.Target != TargetFolder || s.Path == "" {
		return nil
	}
	p := realPath(expandHome(s.Path))
	for _, own := range []string{home, dataDir} {
		if own != "" && within(realPath(own), p) {
			return fmt.Errorf("backups can't go in %s: choose a folder outside Mirrin's own folder (%s), ideally on another disk or a synced drive", s.Path, own)
		}
	}
	return nil
}

// realPath is p with links resolved, as far as it exists.
func realPath(p string) string {
	p, _ = filepath.Abs(p)
	rest := ""
	for dir := p; ; dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(dir) == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// dataDirOf is the data folder the config at cfgPath names.
func dataDirOf(cfgPath string) string {
	return oldDataDir(filepath.Dir(cfgPath))
}

// Where describes a target setting in the owner's words.
func Where(s config.Backup) string {
	switch s.Target {
	case TargetFolder:
		return s.Path
	case "", TargetICloud:
		return icloudWhere("")
	case TargetS3:
		return whereS3(s.S3, "")
	case TargetCloud:
		return cloudWhere
	}
	return s.Target
}

// cloudWhere is how the paid service's target is named.
const cloudWhere = "Mirrin Cloud"

// WhereNS names the folder one set of words' backups are in, in the
// owner's words: the namespace folder inside the target.
func WhereNS(s config.Backup, ns string) string {
	switch s.Target {
	case TargetFolder:
		return filepath.Join(expandHome(s.Path), ns)
	case "", TargetICloud:
		return icloudWhere(ns) + " › " + ns
	case TargetS3:
		return whereS3(s.S3, ns)
	}
	return Where(s) + " › " + ns
}

// NudgeDue reports whether it is time to ask the owner whether they still
// have their words: a year after the kit was shown, then yearly.
func NudgeDue(st State, now time.Time) bool {
	last := st.KitShown
	if st.Nudged.After(last) {
		last = st.Nudged
	}
	if last.IsZero() {
		// No kit was shown here (the keys came with a restored or imported
		// config): count from when backups began here.
		last = st.Since
	}
	return !last.IsZero() && now.Sub(last) >= 365*24*time.Hour
}

// NudgeText is the yearly question.
const NudgeText = "A year ago you wrote down 12 words for your backups. Do you still have them somewhere safe? If not, run `mirrin backup init --new` to get new ones (the old ones keep opening older backups)."
