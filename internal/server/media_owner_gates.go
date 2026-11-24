package server

import (
	"context"
	"crypto/sha256"
	"sync"
)

// The archive and rendition workers are independent. Serialize quota
// snapshots and their media writes for the same owner. Fixed sharding
// bounds memory; a hash collision may conservatively serialize two owners.
// This is not a filesystem quota or a lock for arbitrary external writers.
type mediaOwnerGate struct {
	once sync.Once
	busy chan struct{}
}

type mediaOwnerGates struct {
	shards [64]mediaOwnerGate
}

func (g *mediaOwnerGates) gate(owner string) *mediaOwnerGate {
	hash := sha256.Sum256([]byte(owner))
	return &g.shards[int(hash[0])%len(g.shards)]
}

func (g *mediaOwnerGates) acquire(ctx context.Context, owner string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	shard := g.gate(owner)
	shard.once.Do(func() { shard.busy = make(chan struct{}, 1) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case shard.busy <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-shard.busy
			return nil, err
		}
		var once sync.Once
		return func() { once.Do(func() { <-shard.busy }) }, nil
	}
}

// FFmpeg's output is already on disk. Validate the actual logical quota
// and remaining physical reserve before handing it to the publication
// transaction, rather than subtracting its size from free space twice.
func checkVideoRenditionBudget(ctx context.Context, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return checkCompletedMediaOutputLocked(b, size)
}
