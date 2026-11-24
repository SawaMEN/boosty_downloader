package server

import (
	"context"
	"errors"
	"syscall"
)

func fatalArchiveMediaError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
