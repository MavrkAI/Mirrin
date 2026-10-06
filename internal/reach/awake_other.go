//go:build !darwin && !linux && !windows

package reach

import (
	"context"
	"fmt"
)

func onPower() bool                 { return false }
func inhibit(context.Context) error { return fmt.Errorf("Keep awake isn't available on this system") }
