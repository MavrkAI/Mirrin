package config

import (
	"os"

	"github.com/MavrkAI/Mirrin/internal/brand"
)

// ServiceEnv is set in the environment of a twin the service manager
// (launchd, systemd, Windows) starts. Such a twin waits for another copy
// from the same home to quit rather than exiting, since the manager would
// only start it again. Services AntBot installed set ANTBOT_SERVICE, which
// counts too.
const ServiceEnv = "MIRRIN_SERVICE"

// ServiceMarked reports whether this process carries the service marker,
// under its current name or AntBot's.
func ServiceMarked() bool { return brand.Getenv(ServiceEnv) != "" }

// UnderServiceManager reports whether the service manager started this
// process: the marker a service definition sets, or, for launchd jobs
// installed before the marker existed, the job label launchd passes on
// (mirrin, or antbot for a job AntBot installed).
func UnderServiceManager() bool {
	return ServiceMarked() || brand.IsServiceLabel(os.Getenv("XPC_SERVICE_NAME"))
}
