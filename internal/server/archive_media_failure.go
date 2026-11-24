package server

import (
	"context"
	"errors"
	"syscall"
)

func fatalArchiveMediaError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errStorageUserQuota) || errors.Is(err, errStorageFreeReserve) ||
		errors.Is(err, errStorageBudgetProbe) || errors.Is(err, errMediaMuxLimit)
}
