package server

import (
	"context"
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
	if err := b.checkLocked(size); err != nil {
		return err
	}
	if err := os.Rename(output, dest); err != nil {
		return err
	}
	b.used += size
	return nil
}
