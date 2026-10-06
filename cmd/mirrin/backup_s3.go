package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// s3Options are the options that go with an s3://bucket/folder address.
type s3Options struct {
	endpoint, region, accessEnv, secretEnv string
	pathStyle, virtualHost, set            bool
}

const s3OptionsUsage = `S3 options (with --s3 or target s3): --endpoint <url> --region <name> --path-style | --virtual-host
                  --access-key-env <VAR> --secret-key-env <VAR>  (defaults ` + config.DefaultS3AccessKeyEnv + `, ` + config.DefaultS3SecretKeyEnv + `)`

// take reads the S3 option at args[i], if it is one, and returns the index
// of the last argument it used.
func (o *s3Options) take(args []string, i int) (int, bool, error) {
	a := args[i]
	switch a {
	case "--path-style":
		o.pathStyle, o.set = true, true
		return i, true, nil
	case "--virtual-host":
		o.virtualHost, o.set = true, true
		return i, true, nil
	case "--endpoint", "--region", "--access-key-env", "--secret-key-env":
	default:
		return i, false, nil
	}
	if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
		return i, true, fmt.Errorf("%s needs a value\n%s", a, s3OptionsUsage)
	}
	v := strings.TrimSpace(args[i+1])
	switch a {
	case "--endpoint":
		o.endpoint = v
	case "--region":
		o.region = v
	case "--access-key-env":
		o.accessEnv = v
	case "--secret-key-env":
		o.secretEnv = v
	}
	o.set = true
	return i + 1, true, nil
}

// s3Where is the setting for an s3://bucket/folder address with o.
func (o s3Options) where(raw string) (config.Backup, error) {
	bucket, prefix, err := backup.ParseS3URL(raw)
	if err != nil {
		return config.Backup{}, fmt.Errorf("%v\nGive the bucket as s3://<bucket>/<folder>", err)
	}
	if o.pathStyle && o.virtualHost {
		return config.Backup{}, errors.New("choose one of --path-style and --virtual-host")
	}
	s := config.BackupS3{
		Endpoint: o.endpoint, Region: o.region, Bucket: bucket, Prefix: prefix,
		AccessKeyEnv: o.accessEnv, SecretKeyEnv: o.secretEnv, PathStyle: o.pathStyle,
	}
	// Stores other than AWS (MinIO, R2, B2, Wasabi) take the bucket in the
	// path everywhere; AWS prefers it in the hostname.
	if o.endpoint != "" && !o.virtualHost {
		if u, err := backup.S3Endpoint(o.endpoint, o.region); err == nil && !strings.HasSuffix(u.Hostname(), ".amazonaws.com") {
			s.PathStyle = true
		}
	}
	if _, err := backup.S3Endpoint(s.Endpoint, s.Region); err != nil {
		return config.Backup{}, err
	}
	return config.Backup{Target: backup.TargetS3, S3: s}, nil
}

// s3Keys makes sure the bucket's key pair can be found: from the
// environment or secrets.env, or typed in here and kept in secrets.env
// (save) or for this run only. The keys never go into config.yaml.
func s3Keys(t *terminal, s config.Backup, save bool) error {
	if s.Target != backup.TargetS3 {
		return nil
	}
	access, secret := s.S3.KeyEnvs()
	missing := map[string]string{}
	for _, n := range []string{access, secret} {
		if config.Secret(n) == "" {
			missing[n] = ""
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if !t.tty {
		return fmt.Errorf("the bucket's key isn't set: put its access key id in %s and its secret in %s (export them, or run this in a terminal to type them)", access, secret)
	}
	if _, ok := missing[access]; ok {
		v, err := t.ask("Access key id for the bucket: ")
		if err != nil {
			return err
		}
		missing[access] = v
	}
	if _, ok := missing[secret]; ok {
		fmt.Fprint(t.msg, "Secret access key (not shown as you type): ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(t.msg)
		if err != nil {
			return err
		}
		missing[secret] = strings.TrimSpace(string(b))
	}
	for n, v := range missing {
		if v == "" {
			return fmt.Errorf("no key was typed for %s", n)
		}
	}
	if !save {
		for n, v := range missing {
			if err := os.Setenv(n, v); err != nil {
				return err
			}
		}
		return nil
	}
	if err := config.SaveSecrets(missing); err != nil {
		return err
	}
	fmt.Fprintf(t.msg, "Saved the key in %s (only you can read it), not in config.yaml.\n", config.SecretsPath())
	return nil
}

// prepareS3 finds (or asks for and saves) the bucket's key and tries the
// bucket, for an "s3" setting; other settings pass.
func prepareS3(ctx context.Context, t *terminal, s config.Backup) error {
	if err := s3Keys(t, s, true); err != nil {
		return err
	}
	return checkBucket(ctx, t, s)
}

// s3TargetArgs reads `target s3 s3://bucket/folder [S3 options]`.
func s3TargetArgs(args []string) (config.Backup, error) {
	if len(args) == 0 {
		return config.Backup{}, errors.New("usage: mirrin backup target s3 s3://<bucket>/<folder> [S3 options]\n" + s3OptionsUsage)
	}
	var o s3Options
	for i := 1; i < len(args); i++ {
		j, ok, err := o.take(args, i)
		if err != nil {
			return config.Backup{}, err
		}
		if !ok {
			return config.Backup{}, fmt.Errorf("unknown option %s\n%s", args[i], s3OptionsUsage)
		}
		i = j
	}
	return o.where(args[0])
}

// restoreS3 is where `restore --from s3://…` looks: the address with the
// options given, or else with the store settings this machine already has.
func restoreS3(raw string, o s3Options) (config.Backup, error) {
	if !o.set {
		if s, err := backup.LoadSettings(config.Path()); err == nil && s.Target == backup.TargetS3 {
			o = s3Options{endpoint: s.S3.Endpoint, region: s.S3.Region, accessEnv: s.S3.AccessKeyEnv, secretEnv: s.S3.SecretKeyEnv,
				pathStyle: s.S3.PathStyle, virtualHost: !s.S3.PathStyle}
		}
	}
	return o.where(raw)
}

// checkBucket tries the bucket with its key before anything is changed.
func checkBucket(ctx context.Context, t *terminal, s config.Backup) error {
	if s.Target != backup.TargetS3 {
		return nil
	}
	fmt.Fprintf(t.msg, "Checking %s…\n", backup.Where(s))
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := backup.CheckBucket(cctx, s); err != nil {
		return fmt.Errorf("can't use %s: %v", backup.Where(s), err)
	}
	return nil
}
