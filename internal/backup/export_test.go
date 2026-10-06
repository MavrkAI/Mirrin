package backup

import (
	"context"
	"time"
)

// TuneS3 sets an S3 target's multipart threshold and part size, and makes
// its retries immediate, for tests in package backup_test.
func TuneS3(t Target, threshold, partSize int64) {
	s := t.(*s3Target)
	s.threshold, s.partSize = threshold, partSize
	s.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
}

// S3KeysFrom makes an S3 target read its key pair from secret.
func S3KeysFrom(t Target, secret func(name string) string) {
	t.(*s3Target).secret = secret
}
