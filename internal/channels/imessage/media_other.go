//go:build !darwin

package imessage

import (
	"context"
	"errors"
)

func nativeConvertAttachment(context.Context, []byte, string, int64) ([]byte, error) {
	return nil, errors.New("iMessage attachment conversion requires macOS")
}
