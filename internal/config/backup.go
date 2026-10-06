package config

// Backup configures encrypted backups (internal/backup, `mirrin backup`).
// Only public keys live here: the 12 words that unlock the backups are
// never stored anywhere. An empty Recipient means backups aren't set up.
type Backup struct {
	// Recipient is the age public key snapshots are encrypted to (age1pq1… or age1…).
	Recipient string `yaml:"recipient,omitempty"`
	// RecoveryPub is the Ed25519 recovery public key (base64url, no padding).
	// It checks handover markers and names the backup's folder.
	RecoveryPub string `yaml:"recovery_pub,omitempty"`
	// Target is where snapshots go: "icloud" (iCloud Drive, the macOS
	// default), "folder" (Path), "s3" (S3), or "cloud" (the optional paid
	// service this machine linked with).
	Target string `yaml:"target,omitempty"`
	// Path is the folder for the "folder" target.
	Path string `yaml:"path,omitempty"`
	// S3 is the bucket for the "s3" target.
	S3 BackupS3 `yaml:"s3,omitempty"`
	// Sessions also backs up the WhatsApp session. Off by default: two
	// machines on one linked WhatsApp conflict.
	Sessions bool `yaml:"sessions,omitempty"`
	// Signal also backs up signal-cli's data folder (its linked account),
	// as a tar under sessions/. Off by default, for the same reason: two
	// machines on one linked Signal account conflict.
	Signal bool `yaml:"signal,omitempty"`
	// SignalDir is signal-cli's data folder; empty means its default,
	// ~/.local/share/signal-cli (or $XDG_DATA_HOME/signal-cli).
	SignalDir string `yaml:"signal_dir,omitempty"`
}

// BackupS3 is an S3-compatible bucket (AWS S3, Cloudflare R2, Backblaze
// B2, MinIO, Wasabi). The key pair is never written here: AccessKeyEnv and
// SecretKeyEnv name the variables that hold it, read from the environment
// or secrets.env (Secret).
type BackupS3 struct {
	// Endpoint is the store's address; empty means AWS S3 in Region.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Region signs requests; empty is worked out from the endpoint.
	Region string `yaml:"region,omitempty"`
	Bucket string `yaml:"bucket,omitempty"`
	// Prefix is the folder in the bucket; each set of words gets its own
	// folder inside it.
	Prefix string `yaml:"prefix,omitempty"`
	// PathStyle puts the bucket in the path rather than the hostname.
	PathStyle bool `yaml:"path_style,omitempty"`
	// AccessKeyEnv and SecretKeyEnv default to MIRRIN_S3_ACCESS_KEY_ID and
	// MIRRIN_S3_SECRET_ACCESS_KEY.
	AccessKeyEnv string `yaml:"access_key_env,omitempty"`
	SecretKeyEnv string `yaml:"secret_key_env,omitempty"`
}

// The variables an S3 key pair is read from when the config names none.
const (
	DefaultS3AccessKeyEnv = "MIRRIN_S3_ACCESS_KEY_ID"
	DefaultS3SecretKeyEnv = "MIRRIN_S3_SECRET_ACCESS_KEY"
)

// KeyEnvs are the variables holding the key pair, defaults filled in.
func (s BackupS3) KeyEnvs() (access, secret string) {
	access, secret = s.AccessKeyEnv, s.SecretKeyEnv
	if access == "" {
		access = DefaultS3AccessKeyEnv
	}
	if secret == "" {
		secret = DefaultS3SecretKeyEnv
	}
	return access, secret
}
