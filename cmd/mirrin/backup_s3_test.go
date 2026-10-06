package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/backup/s3fake"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// fakeBucketKeys puts the fake store's key pair in the default variables.
func fakeBucketKeys(t *testing.T) {
	t.Setenv(config.DefaultS3AccessKeyEnv, s3fake.AccessKey)
	t.Setenv(config.DefaultS3SecretKeyEnv, s3fake.SecretKey)
}

func TestBackupToS3AndRestoreFromIt(t *testing.T) {
	twinHome(t)
	srv := s3fake.New(t)
	fakeBucketKeys(t)
	ctx := context.Background()

	p, said := initWith(t, "--s3", "s3://"+s3fake.Bucket+"/home", "--endpoint", srv.URL)
	if !strings.Contains(said, "Backups are on. They go to s3://"+s3fake.Bucket+"/home on 127.0.0.1:") || !strings.Contains(said, "Backed up Jeeves") {
		t.Fatalf("init said:\n%s", said)
	}
	keys := srv.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "home/"+p.Namespace()+"/snap-") {
		t.Fatalf("stored %v", keys)
	}
	raw, _ := os.ReadFile(config.Path())
	for _, want := range []string{"target: s3", "bucket: " + s3fake.Bucket, "prefix: home", "path_style: true", "endpoint: " + srv.URL} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config.yaml lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), s3fake.SecretKey) || strings.Contains(string(raw), s3fake.AccessKey) {
		t.Fatalf("config.yaml holds the bucket's key:\n%s", raw)
	}

	term, out, _ := testTerminal(strings.NewReader(""))
	if err := runBackup(ctx, term, []string{"now"}); err != nil || !strings.Contains(out.String(), "Backed up") {
		t.Fatalf("now: %v %s", err, out)
	}
	words := strings.Join(p.Words(), " ") + "\n"
	term, out, _ = testTerminal(strings.NewReader(words))
	if err := runBackup(ctx, term, []string{"list"}); err != nil || strings.Count(out.String(), "snap-") != 2 {
		t.Fatalf("list: %v\n%s", err, out)
	}
	term, out, _ = testTerminal(strings.NewReader(words))
	if err := runBackup(ctx, term, []string{"verify"}); err != nil || !strings.Contains(out.String(), "is intact: #2") {
		t.Fatalf("verify: %v\n%s", err, out)
	}

	// A new machine that only has the words, the address and the key.
	t.Setenv("MIRRIN_HOME", t.TempDir())
	term, out, _ = testTerminal(strings.NewReader(words))
	if err := runRestore(ctx, term, []string{"--from", "s3://" + s3fake.Bucket + "/home", "--endpoint", srv.URL}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(out.String(), "Restored Jeeves") || !strings.Contains(out.String(), "#2") {
		t.Fatalf("restore said:\n%s", out)
	}
	restored, _ := config.Load()
	if restored == nil || restored.Backup.Target != backup.TargetS3 || restored.Backup.S3.Bucket != s3fake.Bucket {
		t.Fatalf("restored backup settings: %+v", restored)
	}
}

// Without the key, or with a wrong one, nothing is set up and no words are
// shown; the message names the variables to set.
func TestInitS3ChecksTheKeyBeforeTheWords(t *testing.T) {
	twinHome(t)
	srv := s3fake.New(t)
	t.Setenv(config.DefaultS3AccessKeyEnv, "")
	t.Setenv(config.DefaultS3SecretKeyEnv, "")
	term, out, _ := testTerminal(strings.NewReader(""))
	err := runBackup(context.Background(), term, []string{"init", "--s3", "s3://" + s3fake.Bucket, "--endpoint", srv.URL})
	if err == nil || !strings.Contains(err.Error(), config.DefaultS3AccessKeyEnv) || !strings.Contains(err.Error(), config.DefaultS3SecretKeyEnv) || strings.Contains(out.String(), "Recovery Kit") {
		t.Fatalf("no key: %v\n%s", err, out)
	}

	t.Setenv("R2_ID", s3fake.AccessKey)
	t.Setenv("R2_SECRET", "not-the-secret")
	term, out, _ = testTerminal(strings.NewReader(""))
	err = runBackup(context.Background(), term, []string{"init", "--s3", "s3://" + s3fake.Bucket, "--endpoint", srv.URL,
		"--access-key-env", "R2_ID", "--secret-key-env", "R2_SECRET"})
	if err == nil || !strings.Contains(err.Error(), "R2_SECRET") || !strings.Contains(err.Error(), "doesn't match") || strings.Contains(out.String(), "Recovery Kit") {
		t.Fatalf("wrong secret: %v\n%s", err, out)
	}
	if s, _ := backup.LoadSettings(config.Path()); s.Recipient != "" || s.Target != "" {
		t.Fatalf("settings changed: %+v", s)
	}
}

func TestBackupTargetS3(t *testing.T) {
	twinHome(t)
	srv := s3fake.New(t)
	fakeBucketKeys(t)
	term, out, _ := testTerminal(strings.NewReader(""))
	if err := runBackup(context.Background(), term, []string{"target", "s3", "s3://" + s3fake.Bucket + "/a/b", "--endpoint", srv.URL, "--region", s3fake.Region}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Backups now go to s3://"+s3fake.Bucket+"/a/b on ") {
		t.Fatalf("said %q", out)
	}
	s, _ := backup.LoadSettings(config.Path())
	if s.Target != backup.TargetS3 || s.S3.Prefix != "a/b" || s.S3.Region != s3fake.Region || !s.S3.PathStyle {
		t.Fatalf("settings %+v", s)
	}
	cfg, _ := config.Load()
	if envs := strings.Join(cfg.SecretEnvs(), " "); !strings.Contains(envs, config.DefaultS3SecretKeyEnv) {
		t.Fatalf("the service wouldn't be given the key: %s", envs)
	}
}

func TestS3OptionMistakes(t *testing.T) {
	twinHome(t)
	for _, args := range [][]string{
		{"init", "--endpoint", "https://minio.lan"},
		{"init", "--s3"},
		{"init", "--s3", "my-bucket"},
		{"init", "--s3", "s3://b/../x"},
		{"init", "--s3", "s3://bucket", "--region"},
		{"init", "--s3", "s3://bucket", "--path-style", "--virtual-host"},
		{"init", "--s3", "s3://bucket", "--folder", "/tmp/x"},
		{"target", "s3"},
		{"target", "s3", "s3://bucket", "--bogus"},
	} {
		term, out, _ := testTerminal(strings.NewReader(""))
		if err := runBackup(context.Background(), term, args); err == nil || strings.Contains(out.String(), "Recovery Kit") {
			t.Errorf("%v: %v %s", args, err, out)
		}
	}
	if _, err := parseRestore([]string{"--from", "/some/folder", "--endpoint", "https://x"}); err == nil {
		t.Error("restore took S3 options for a folder")
	}
}

// A store that isn't AWS gets path-style addresses unless asked otherwise.
func TestS3OptionsPathStyle(t *testing.T) {
	for _, tc := range []struct {
		o    s3Options
		path bool
	}{
		{s3Options{}, false},
		{s3Options{endpoint: "https://s3.eu-west-1.amazonaws.com"}, false},
		{s3Options{endpoint: "https://acct.r2.cloudflarestorage.com"}, true},
		{s3Options{endpoint: "https://s3.us-west-004.backblazeb2.com", virtualHost: true}, false},
		{s3Options{pathStyle: true}, true},
	} {
		w, err := tc.o.where("s3://bucket/x")
		if err != nil || w.S3.PathStyle != tc.path {
			t.Errorf("%+v: path style %v (%v), want %v", tc.o, w.S3.PathStyle, err, tc.path)
		}
	}
}
