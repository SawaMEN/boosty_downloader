package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type mediaSegmentTask struct {
	url, path      string
	offset, length int64
}

type mediaSegmentResult struct {
	index int
	size  int64
	err   error
}

// At most two workers, both joined before returning. Each parallel task
// receives the full maximum segment size only if both fit the remaining
// archive budget. The existing disk-write gate accounts for staged bytes.
func fetchMediaSegmentBatch(ctx context.Context, fetch mediaFetcher, tasks []mediaSegmentTask, budget int64) ([2]int64, error) {
	var sizes [2]int64
	if len(tasks) == 1 {
		var err error
		sizes[0], err = fetchHLSRangeSegment(ctx, fetch, tasks[0].url, tasks[0].path, budget, tasks[0].offset, tasks[0].length)
		return sizes, err
	}
	if len(tasks) != 2 || budget < 2*maxHLSSegmentBytes {
		return sizes, errors.New("недопустимый параллельный пакет медиасегментов")
	}
	// At most one extra segment worker across this process. Never wait
	// for another track: a busy gate falls back to ordered single requests.
	n, _ := ctx.Value(segmentWorkersKey{}).(int)
	allow := n != 3 || adaptiveSegmentWorkers() == 2
	if allow {
		select {
		case mediaExtraSegmentGate <- struct{}{}:
			defer func() { <-mediaExtraSegmentGate }()
		default:
			allow = false
		}
	}
	if !allow {
		for i, task := range tasks {
			size, err := fetchHLSRangeSegment(ctx, fetch, task.url, task.path, budget, task.offset, task.length)
			if err != nil {
				return sizes, err
			}
			sizes[i] = size
			budget -= size
		}
		return sizes, nil
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan mediaSegmentResult, 2)
	for i, task := range tasks {
		go func(i int, task mediaSegmentTask) {
			n, err := fetchHLSRangeSegment(child, fetch, task.url, task.path, maxHLSSegmentBytes, task.offset, task.length)
			results <- mediaSegmentResult{index: i, size: n, err: err}
		}(i, task)
	}
	var first error
	for range tasks {
		result := <-results
		sizes[result.index] = result.size
		if result.err != nil {
			if first == nil || (!errors.Is(result.err, context.Canceled) && fatalArchiveMediaError(result.err)) {
				first = result.err
			}
			cancel()
		}
	}
	if err := ctx.Err(); err != nil {
		return sizes, err
	}
	return sizes, first
}

func appendDASHSegment(ctx context.Context, output io.Writer, temp string, size int64) error {
	part, err := openArchiveRegular(filepath.Dir(temp), filepath.Base(temp))
	if err != nil {
		return err
	}
	n, cpErr := guardedCopy(ctx, output, io.LimitReader(part, size+1))
	closeErr := part.Close()
	if err := os.Remove(temp); err == nil {
		releaseStorageBudget(ctx, size)
	}
	if cpErr != nil {
		return cpErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != size {
		return errMediaIncomplete
	}
	return nil
}
