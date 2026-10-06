package reach

import (
	"context"
	"github.com/MavrkAI/Mirrin/internal/procenv"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Tests point these at fakes.
var (
	pmsetPath      = "/usr/bin/pmset"
	caffeinatePath = "/usr/bin/caffeinate"
)

func onPower() bool {
	cmd := exec.Command(pmsetPath, "-g", "batt")
	cmd.Env = procenv.Base()
	b, e := cmd.Output()
	return e == nil && strings.Contains(string(b), "AC Power")
}
func inhibit(ctx context.Context) error {
	return runPowerCommand(ctx, caffeinatePath, "-s", "-w", strconv.Itoa(os.Getpid()))
}

func runPowerCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = procenv.Base()
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}
