// Package brand is the product's name in the forms the code needs: the
// command and service label, the name people read and the home folder.
package brand

const (
	// Name is the command, the service label and the lower-case name.
	Name = "mirrin"
	// DisplayName is the name people read.
	DisplayName = "Mirrin"
	// HomeDirName is the home folder's name in the user's home (~/.mirrin).
	HomeDirName = ".mirrin"
)

// IsServiceLabel reports whether a launchd job label (launchd passes it on
// as XPC_SERVICE_NAME) is the product's background service.
func IsServiceLabel(label string) bool { return label == Name }
