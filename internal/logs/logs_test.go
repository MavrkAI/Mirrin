package logs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

func TestWriterRollsOverAndKeepsAFewFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirrin.log")
	w, err := NewWriter(path, 1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 100; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	var total int64
	for _, p := range []string{path, path + ".1", path + ".2"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s missing: %v", p, err)
		}
		if st.Size() > 1000 {
			t.Fatalf("%s is %d bytes, over the cap", p, st.Size())
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v, want 0600", p, st.Mode().Perm())
		}
		total += st.Size()
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("kept more rolled files than asked")
	}
	if total < 2000 {
		t.Fatalf("lost too much: %d bytes across files", total)
	}
}

// Two processes writing one log (the menu bar app and a terminal chat) don't
// roll it twice or lose each other's lines to a stale file.
func TestWritersShareAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirrin.log")
	a, _ := NewWriter(path, 2000, 3)
	b, _ := NewWriter(path, 2000, 3)
	var wg sync.WaitGroup
	for _, w := range []*Writer{a, b} {
		wg.Add(1)
		go func(w *Writer) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				_, _ = w.Write([]byte(strings.Repeat("y", 49) + "\n"))
			}
		}(w)
	}
	wg.Wait()
	a.Close()
	b.Close()
	lines, err := TailFiles(1000, path+".3", path+".2", path+".1", path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 60 {
		t.Fatalf("want all 60 lines kept across the files, got %d", len(lines))
	}
}

func TestRedactorHidesSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	if err := config.SaveSecrets(map[string]string{"ZULIP_API_KEY": "zulip-secret-value-123"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, "data", "api.token"), []byte("f00dfeedcafe1234\n"), 0o600)
	t.Setenv("MY_SERVICE_TOKEN", "envtokenvalue99")
	r := NewRedactor(home)
	cases := map[string]string{
		`Get "https://api.telegram.org/bot123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/getUpdates": dial tcp`: "api.telegram.org/bot[hidden]/getUpdates",
		"anthropic 401: invalid x-api-key sk-ant-api03-abcdefghijklmnopqrstuv":                                "invalid x-api-key [hidden]",
		"openai key sk-proj-ABCDEFGHIJKLMNOPQRSTUVWX rejected":                                                "openai key [hidden] rejected",
		"zulip said zulip-secret-value-123 was wrong":                                                         "zulip said [hidden] was wrong",
		"cookie was f00dfeedcafe1234":                                                                         "cookie was [hidden]",
		"env envtokenvalue99":                                                                                 "env [hidden]",
		"Authorization: Bearer abc.def.ghijklmnop":                                                            "Bearer [hidden]",
		"GET https://x.example/cb?code=1&access_token=zzzzzzzz&state=2":                                       "access_token=[hidden]&state=2",
		"https://tony:hunter2hunter2@mail.example.com/":                                                       "https://tony:[hidden]@mail.example.com/",
		"slack xoxb-1234567890-abcdefghij":                                                                    "slack [hidden]",
		"password: correct-horse":                                                                             "password: [hidden]",
	}
	for in, want := range cases {
		got := r.String(in)
		if !strings.Contains(got, want) {
			t.Errorf("%q\n  became %q\n  want it to contain %q", in, got, want)
		}
	}
	// What isn't a secret stays readable.
	for _, s := range []string{
		"telegram:1#task-0927-morning-brief-with-a-long-name",
		"max_tokens=16000 api_key_env=ANTHROPIC_API_KEY",
		"protocol failed: no such tool calendar_list",
	} {
		if got := r.String(s); got != s {
			t.Errorf("%q was changed to %q", s, got)
		}
	}
}

func TestLoggerWritesFileRedactedAndFiltersConsole(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	var console bytes.Buffer
	log, closer, err := New(Options{
		Home: home, Level: slog.LevelInfo,
		Console: &console, ConsoleLevel: slog.LevelWarn,
		ConsoleAlways: func(msg string) bool { return msg == "Mirrin online" },
		Redactor:      NewRedactorWith("hunter2-hunter2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("Mirrin online", "model", "claude-opus-5")
	log.Info("routine detail")
	log.Debug("not kept")
	log.With("api_key", "abc123456").Warn("login failed", "err", errors.New("password hunter2-hunter2 rejected"), "token", "zzzzzz", "tokens", 12)
	log.Error("send", "err", fmt.Errorf("wrap: %w", errors.New("Bearer abcdefghijkl")))
	log.ErrorContext(FileOnly(context.Background()), "stopped", "err", "already shown on the console")
	if log.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("a debug record would be redacted only to be dropped")
	}
	closer.Close()

	b, _ := os.ReadFile(Path(home))
	file := string(b)
	for _, want := range []string{"Mirrin online", "routine detail", "already shown on the console", "login failed", "api_key=[hidden]", "token=[hidden]", "tokens=12", "password [hidden] rejected", "Bearer [hidden]"} {
		if !strings.Contains(file, want) {
			t.Errorf("log file lacks %q:\n%s", want, file)
		}
	}
	for _, leak := range []string{"hunter2", "abc123456", "zzzzzz", "abcdefghijkl", "not kept"} {
		if strings.Contains(file, leak) {
			t.Errorf("log file has %q:\n%s", leak, file)
		}
	}
	con := console.String()
	if !strings.Contains(con, "Mirrin online") || !strings.Contains(con, "login failed") || strings.Contains(con, "routine detail") ||
		strings.Contains(con, "hunter2") || strings.Contains(con, "already shown") {
		t.Errorf("console got:\n%s", con)
	}
}

func TestTailReachesIntoTheRolledFile(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(Dir(home), 0o700)
	_ = os.WriteFile(Path(home)+".1", []byte("one\ntwo\nthree\n"), 0o600)
	_ = os.WriteFile(Path(home), []byte("four\nfive\n"), 0o600)
	got, err := Tail(home, 4)
	if err != nil || strings.Join(got, ",") != "two,three,four,five" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := Tail(t.TempDir(), 5); !os.IsNotExist(err) {
		t.Fatalf("no log yet: %v", err)
	}
}

func TestCaptureCrashesMakesTheFile(t *testing.T) {
	home := t.TempDir()
	if err := CaptureCrashes(home); err != nil {
		t.Fatal(err)
	}
	// The runtime keeps the file open; Windows can't delete it until let go.
	t.Cleanup(func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) })
	if st, err := os.Stat(CrashPath(home)); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("crash log: %v %v", st, err)
	}
}

// Where the live file can't be renamed (Windows, while another process has
// it open), writing goes on and rolling is tried again now and then, not on
// every write.
func TestWriterBacksOffWhenTheFileCantBeRolled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirrin.log")
	var tries int
	stuck := true
	renameFile = func(from, to string) error {
		if from == path {
			tries++
			if stuck {
				return errors.New("the process cannot access the file because it is being used by another process")
			}
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renameFile = os.Rename })
	w, err := NewWriter(path, 800, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := []byte(strings.Repeat("x", 19) + "\n")
	for i := 0; i < 250; i++ { // 5000 bytes: six times the cap
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := os.Stat(path); st.Size() != 5000 {
		t.Fatalf("every write must land, got %d bytes", st.Size())
	}
	// 210 writes past the cap, with a retry every 100 bytes (an eighth of 800).
	if tries == 0 || tries > 45 {
		t.Fatalf("tried to roll %d times", tries)
	}
	before := tries
	for i := 0; i < 4; i++ {
		_, _ = w.Write(line)
	}
	if tries-before > 1 {
		t.Fatalf("retries don't back off: %d more", tries-before)
	}
	stuck = false
	for i := 0; i < 6; i++ {
		_, _ = w.Write(line)
	}
	if st, _ := os.Stat(path); st.Size() > 800 {
		t.Fatalf("once it can, the file rolls: %d bytes", st.Size())
	}
}
