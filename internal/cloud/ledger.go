package cloud

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The egress ledger is data/cloud/egress.jsonl: one JSON line for every
// request this machine sends to Mirrin Cloud. It records sizes and the path,
// never a body, a query or a header, so `mirrin cloud egress` can show what
// left and when without itself becoming something worth stealing.
const (
	ledgerFile = "egress.jsonl"
	// ledgerMax is the size at which the file moves to egress.jsonl.1,
	// replacing the older one. A daily refresh writes about 40 KB a year.
	ledgerMax = 1 << 20
	// ledgerLineMax bounds one line on read.
	ledgerLineMax = 4096
)

// Entry is one request in the ledger.
type Entry struct {
	At        time.Time `json:"at"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	ReqBytes  int64     `json:"req_bytes"`  // request body
	RespBytes int64     `json:"resp_bytes"` // response body
	Status    int       `json:"status"`     // 0 when no answer came back
}

// ledgerMu keeps appends from one process whole.
var ledgerMu sync.Mutex

// openLedger opens the ledger for one append, creating data/cloud if need
// be. The client opens it before a request is sent, so a machine that
// cannot write its ledger sends nothing.
func openLedger(dataDir string) (*os.File, error) {
	dir, err := cloudDir(dataDir, true)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, ledgerFile)
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	if fi, err := os.Stat(p); err == nil && fi.Size() >= ledgerMax {
		if err := os.Rename(p, p+".1"); err != nil {
			return nil, fmt.Errorf("cloud: rotating the egress ledger: %w", err)
		}
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cloud: opening the egress ledger: %w", err)
	}
	return f, nil
}

// writeEntry appends e to f as one line.
func writeEntry(f io.Writer, e Entry) error {
	e.At = e.At.UTC().Truncate(time.Second)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	_, err = f.Write(append(b, '\n'))
	return err
}

// RecordEgress appends e to the ledger in dataDir. The client records its
// own requests; other code that contacts Mirrin Cloud or its relays on this
// machine's behalf records here too.
func RecordEgress(dataDir string, e Entry) error {
	if _, err := checkEntry(e); err != nil {
		return err
	}
	f, err := openLedger(dataDir)
	if err != nil {
		return err
	}
	err = writeEntry(f, e)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadLedger returns every entry, oldest first, and how many lines it could
// not read (a line torn by a crash, say). A machine that never sent
// anything has no ledger, and gets no entries and no error.
func ReadLedger(dataDir string) ([]Entry, int, error) {
	dir, err := cloudDir(dataDir, false)
	if err != nil {
		return nil, 0, err
	}
	var all []Entry
	bad := 0
	for _, name := range []string{ledgerFile + ".1", ledgerFile} {
		es, n, err := readLedgerFile(filepath.Join(dir, name))
		if err != nil {
			return nil, 0, err
		}
		all, bad = append(all, es...), bad+n
	}
	return all, bad, nil
}

func readLedgerFile(p string) ([]Entry, int, error) {
	b, err := readFileMax(p, 2*ledgerMax)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var es []Entry
	bad := 0
	for line := range bytes.SplitSeq(b, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		e, err := parseEntry(line)
		if err != nil {
			bad++
			continue
		}
		es = append(es, e)
	}
	return es, bad, nil
}

// parseEntry reads one ledger line strictly.
func parseEntry(line []byte) (Entry, error) {
	if len(line) > ledgerLineMax {
		return Entry{}, errors.New("cloud: ledger line too long")
	}
	var e Entry
	if err := json.Unmarshal(line, &e, json.RejectUnknownMembers(true)); err != nil {
		return Entry{}, err
	}
	return checkEntry(e)
}

// checkEntry holds the rules every entry meets.
func checkEntry(e Entry) (Entry, error) {
	switch {
	case e.At.IsZero():
		return Entry{}, errors.New("cloud: ledger entry without a time")
	case e.Method == "" || len(e.Method) > 16 || e.Host == "" || len(e.Host) > 256 || e.Path == "" || len(e.Path) > 1024:
		return Entry{}, errors.New("cloud: ledger entry without a method, host or path")
	case e.ReqBytes < 0 || e.RespBytes < 0 || e.Status < 0 || e.Status > 999:
		return Entry{}, errors.New("cloud: ledger entry with a bad size or status")
	}
	return e, nil
}
