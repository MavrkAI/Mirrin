package reach

import (
	"context"
	"github.com/MavrkAI/Mirrin/internal/procenv"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func onPower() bool { return powerSupplyOnAC("/sys/class/power_supply") }
func inhibit(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "systemd-inhibit", "--what=sleep", "--mode=block", "--who=Mirrin", "--why=Stay reachable while on power", "--", "tail", "--pid="+strconv.Itoa(os.Getpid()), "-f", "/dev/null")
	cmd.Env = procenv.Base()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}
