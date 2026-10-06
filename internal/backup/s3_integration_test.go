//go:build integration

// The S3 target against a real S3-compatible server, such as a local MinIO:
//
//	CGO_ENABLED=0 go install github.com/minio/minio@RELEASE.2025-04-22T22-12-26Z
//	MINIO_ROOT_USER=mirrin MINIO_ROOT_PASSWORD=mirrin-secret minio server --address 127.0.0.1:9000 /tmp/minio &
//	curl -X PUT --aws-sigv4 aws:amz:us-east-1:s3 --user mirrin:mirrin-secret http://127.0.0.1:9000/mirrin-it
//	MIRRIN_S3_IT_ENDPOINT=http://127.0.0.1:9000 MIRRIN_S3_IT_BUCKET=mirrin-it \
//	MIRRIN_S3_IT_ACCESS_KEY=mirrin MIRRIN_S3_IT_SECRET_KEY=mirrin-secret \
//	MIRRIN_HOME=$(mktemp -d) go test -tags integration -run Integration ./internal/backup/
//
// Point it only at a server and bucket made for the test: every run writes
// under a fresh folder and deletes what it wrote. It skips when the
// variables aren't set, so the tag alone never reaches a real service
// (CI's s3-minio job sets MIRRIN_S3_IT_REQUIRED so it fails instead).
package backup_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/backup/targettest"
)

func TestS3IntegrationConformance(t *testing.T) {
	endpoint, bucket := os.Getenv("MIRRIN_S3_IT_ENDPOINT"), os.Getenv("MIRRIN_S3_IT_BUCKET")
	if endpoint == "" || bucket == "" || os.Getenv("MIRRIN_S3_IT_ACCESS_KEY") == "" {
		const need = "set MIRRIN_S3_IT_ENDPOINT, MIRRIN_S3_IT_BUCKET, MIRRIN_S3_IT_ACCESS_KEY and MIRRIN_S3_IT_SECRET_KEY"
		if os.Getenv("MIRRIN_S3_IT_REQUIRED") != "" {
			t.Fatal(need) // CI started a store for this; a skip would hide it
		}
		t.Skip(need)
	}
	run := time.Now().UTC().Format("20060102T150405")
	n := 0
	targettest.RunWith(t, func(t *testing.T) backup.Target {
		n++
		tg := backup.S3(backup.S3Config{
			Endpoint: endpoint, Region: os.Getenv("MIRRIN_S3_IT_REGION"), Bucket: bucket,
			Prefix:       fmt.Sprintf("mirrin-it/%s/%d", run, n),
			AccessKeyEnv: "MIRRIN_S3_IT_ACCESS_KEY", SecretKeyEnv: "MIRRIN_S3_IT_SECRET_KEY",
			PathStyle: os.Getenv("MIRRIN_S3_IT_VIRTUAL_HOST") == "",
		}, nil)
		t.Cleanup(func() {
			ctx := context.Background()
			objs, _ := tg.List(ctx)
			for _, o := range objs {
				_ = tg.Delete(ctx, o.Name)
			}
		})
		return tg
	}, targettest.Options{Large: 80 << 20, Huge: true})
}
