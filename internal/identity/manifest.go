package identity

// KindBackup marks the manifest of an encrypted backup (internal/backup).
// Identity archives leave Kind empty, as they always have.
const KindBackup = "backup"

// ManifestFile is one file in a backup: its path in the archive (slash
// separated, relative to the twin's home), its size and its SHA-256 (hex).
// A restore checks every file against it.
type ManifestFile struct {
	Path   string `yaml:"path" json:"path"`
	Size   int64  `yaml:"size" json:"size"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}
