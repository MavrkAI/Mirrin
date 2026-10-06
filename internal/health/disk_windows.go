//go:build windows

package health

import "errors"

func diskFree(string) (uint64, error) { return 0, errors.New("not measured on windows") }
