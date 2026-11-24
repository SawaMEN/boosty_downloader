package server

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type mediaStageBudgetKey struct{}

// Tracks only bytes actually charged by guardedWriter in one private remux
// directory. Untracked FFmpeg intermediates are not credited on cleanup.
type mediaStageBudget struct {
	bytes int64 // protected by the parent storageBudget.mu
}

func withMediaStageBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, mediaStageBudgetKey{}, &mediaStageBudget{})
}

func withoutMediaStageBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, mediaStageBudgetKey{}, (*mediaStageBudget)(nil))
}

// Credit charged temporary bytes only once and only after successful removal.
// The per-download segment cache is a sibling, not part of this directory.
func cleanupMediaStage(ctx context.Context, dir string) {
	if err := os.RemoveAll(dir); err != nil {
		return
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	scope, _ := ctx.Value(mediaStageBudgetKey{}).(*mediaStageBudget)
	if b == nil || scope == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= min(b.used, scope.bytes)
	scope.bytes = 0
}

// FFmpeg bypasses guardedWriter. Charge the verified final file before
// publishing it, separately from temporary downloads. Its bytes remain
// charged after cleanup. Existing destinations are not credited here.
func publishStagedVideo(ctx context.Context, output, dest string, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	if b == nil {
		return os.Rename(output, dest)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := checkCompletedMediaOutputLocked(b, size); err != nil {
		return err
	}
	if err := os.Rename(output, dest); err != nil {
		return err
	}
	b.used += size
	return nil
}

// A direct download may inherit a partial from a previous process. Credit
// only this scope's charged bytes, never arbitrary preexisting file sizes.
func discardMediaPartial(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("необычный тип частичного файла")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if scope, _ := ctx.Value(mediaStageBudgetKey{}).(*mediaStageBudget); scope != nil {
		releaseStorageBudget(ctx, info.Size())
	}
	return nil
}

// A completed FFmpeg output already consumes physical disk space. Check
// actual free reserve and logical quota without counting its bytes twice
// against physical free space.
func checkCompletedMediaOutputLocked(b *storageBudget, size int64) error {
	if size <= 0 {
		return errors.New("недопустимый размер видео")
	}
	if err := b.checkLocked(0); err != nil {
		return err
	}
	if b.maxUser > 0 && size > b.maxUser-b.used {
		return fmt.Errorf("%w (%d ГиБ)", errStorageUserQuota, b.maxUser/gibibyte)
	}
	return nil
}

func chargeIntermediateMediaOutput(ctx context.Context, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	if b == nil {
		return nil
	}
	scope, _ := ctx.Value(mediaStageBudgetKey{}).(*mediaStageBudget)
	if scope == nil {
		return errors.New("отсутствует учёт временного видео")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := checkCompletedMediaOutputLocked(b, size); err != nil {
		return err
	}
	b.used += size
	scope.bytes += size
	return nil
}
