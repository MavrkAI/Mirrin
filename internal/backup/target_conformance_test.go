package backup_test

import (
	"testing"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/backup/s3fake"
	"github.com/MavrkAI/Mirrin/internal/backup/targettest"
)

// fakeKeys holds the fake store's key pair under the default variables.
func fakeKeys(name string) string {
	switch name {
	case backup.DefaultS3AccessKeyEnv:
		return s3fake.AccessKey
	case backup.DefaultS3SecretKeyEnv:
		return s3fake.SecretKey
	}
	return ""
}

func TestFolderConformance(t *testing.T) {
	targettest.Run(t, func(t *testing.T) backup.Target { return backup.Folder(t.TempDir()) })
}

// fakeTarget is an S3 target on a fresh fake store, uploading in parts
// above 6 MiB so the suite's 12 MiB round trips go up in three.
func fakeTarget(t *testing.T, pathStyle bool) (backup.Target, *s3fake.Server) {
	srv := s3fake.New(t)
	srv.MaxKeys = 2 // lists come in pages
	endpoint := "http://s3.fake.test:" + srv.Port()
	if pathStyle {
		endpoint = srv.URL
	}
	tg := backup.S3(backup.S3Config{Endpoint: endpoint, Bucket: s3fake.Bucket, Prefix: "twins/ns1", PathStyle: pathStyle}, srv.Client())
	backup.S3KeysFrom(tg, fakeKeys)
	backup.TuneS3(tg, 6<<20, s3fake.MinPartSize)
	return tg, srv
}

func TestS3Conformance(t *testing.T) {
	for _, pathStyle := range []bool{true, false} {
		name := "virtual host"
		if pathStyle {
			name = "path style"
		}
		t.Run(name, func(t *testing.T) {
			targettest.RunWith(t, func(t *testing.T) backup.Target {
				tg, _ := fakeTarget(t, pathStyle)
				return tg
			}, targettest.Options{Large: 12<<20 + 12345})
		})
	}
}

// A 200 MiB snapshot goes up in 16 MiB parts at the real sizes, from a
// stream and from a file, and comes back intact.
func TestS3HugeMultipart(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	for _, fromFile := range []bool{false, true} {
		srv := s3fake.New(t)
		tg := backup.S3(backup.S3Config{Endpoint: srv.URL, Bucket: s3fake.Bucket, Prefix: "twins", PathStyle: true}, srv.Client())
		backup.S3KeysFrom(tg, fakeKeys)
		targettest.RoundTrip(t, tg, targettest.Name("snap", 1), targettest.HugeSize, fromFile)
		if got, want := srv.Count("PART"), 13; got != want {
			t.Errorf("fromFile=%v: %d parts, want %d", fromFile, got, want)
		}
		if srv.Count("PUT") != 0 || srv.PendingUploads() != 0 {
			t.Errorf("fromFile=%v: %d single PUTs, %d uploads left open", fromFile, srv.Count("PUT"), srv.PendingUploads())
		}
	}
}
