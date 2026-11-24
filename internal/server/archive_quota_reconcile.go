package server

import (
	"context"
	"fmt"
)

// Called only at quiescent archive boundaries under the owner's gate.
// There must be no active media stage accounting when the baseline resets.
// The scan includes both live and staged paths; hardlinks are deliberately
// counted logically rather than deduplicated by inode.
func reconcileArchiveStorageBudget(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxUser > 0 {
		used, err := ownerStorageTreeBytes(ctx, b.root, "library", b.owner)
		if err != nil {
			return fmt.Errorf("%w: %w", errStorageBudgetProbe, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		b.used, b.initialized = used, true
	}
	return b.checkLocked(0)
}
