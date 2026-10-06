package reach

import (
	"context"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")

func onPower() bool {
	var status struct {
		AC, Battery, Percent, Flag byte
		Life, Full                 uint32
	}
	r, _, _ := kernel.NewProc("GetSystemPowerStatus").Call(uintptr(unsafe.Pointer(&status)))
	return r != 0 && status.AC == 1
}
func inhibit(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := kernel.NewProc("SetThreadExecutionState")
	r, _, _ := p.Call(0x80000001)
	if r == 0 {
		return fmt.Errorf("Can't keep this computer awake. Check Windows power settings")
	}
	defer p.Call(0x80000000)
	<-ctx.Done()
	return nil
}
