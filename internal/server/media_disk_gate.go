package server

import (
	"context"
	"sync"
)

// One process serves a single data directory. Coordinate ordinary guarded
// writes across owners, not whole downloads. This cannot reserve space
// against subprocesses, unguarded metadata or external writers.
var mediaDiskWriteGate = make(chan struct{}, 1)

func acquireMediaDiskWrite(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case mediaDiskWriteGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-mediaDiskWriteGate
			return nil, err
		}
		var once sync.Once
		return func() { once.Do(func() { <-mediaDiskWriteGate }) }, nil
	}
}
