package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

const mediaMuxPollInterval = 100 * time.Millisecond

var errMediaMuxLimit = errors.New("FFmpeg: превышен допустимый размер или резерв диска")

// The output does not exist yet. Calculate its maximum allowed size from
// the archive cap, current logical user quota and actual free disk space.
// This is an upper bound, not a reservation against concurrent workers.
func mediaMuxOutputLimit(ctx context.Context, maximum int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if maximum <= 0 || maximum > maxArchiveMediaSize {
		return 0, errMediaMuxLimit
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	if b == nil {
		return maximum, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkLocked(0); err != nil {
		return 0, err
	}
	if b.maxUser > 0 {
		maximum = min(maximum, b.maxUser-b.used)
	}
	if b.minFree > 0 {
		_, free, err := filesystemBytes(b.root)
		if err != nil {
			return 0, err
		}
		maximum = min(maximum, free-b.minFree)
	}
	if maximum <= 0 {
		return 0, errMediaMuxLimit
	}
	return maximum, nil
}

func checkMediaMuxOutput(ctx context.Context, path string, limit int64, finished bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && !finished {
		return checkStorageBudget(ctx, 0)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit || (finished && info.Size() <= 0) {
		return errMediaMuxLimit
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkLocked(0); err != nil {
		return err
	}
	if b.maxUser > 0 && info.Size() > b.maxUser-b.used {
		return errMediaMuxLimit
	}
	return nil
}

// Only server-generated local arguments and a private output path are
// passed here. Kill and reap FFmpeg on a quota violation, cancellation or
// timeout. A final check catches a fast write between polling intervals.
// Polling bounds detection latency, not the exact maximum bytes written.
func runBoundedMediaMux(ctx context.Context, ffmpeg string, args []string, output string, maximum int64) error {
	limit, err := mediaMuxOutputLimit(ctx, maximum)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return errors.New("FFmpeg: временный выход уже существует или недоступен")
	}
	muxCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(muxCtx, ffmpeg, args...)
	// Do not buffer arbitrary subprocess diagnostics in memory or expose
	// local paths in an archive error. os/exec discards nil stdout/stderr.
	if err := cmd.Start(); err != nil {
		return errors.New("FFmpeg: не удалось запустить локальную сборку")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(mediaMuxPollInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if muxCtx.Err() != nil {
				return muxCtx.Err()
			}
			if checkErr := checkMediaMuxOutput(muxCtx, output, limit, true); checkErr != nil {
				return checkErr
			}
			if err != nil {
				return errors.New("FFmpeg: ошибка локальной сборки")
			}
			return nil
		case <-muxCtx.Done():
			cancel()
			<-done
			return muxCtx.Err()
		case <-ticker.C:
			if err := checkMediaMuxOutput(muxCtx, output, limit, false); err != nil {
				cancel()
				<-done
				return err
			}
		}
	}
}
