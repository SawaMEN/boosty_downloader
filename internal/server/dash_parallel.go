package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type dashSegmentTask struct {
	url, path string
}

type dashSegmentResult struct {
	index int
	size  int64
	err   error
}

// At most two workers, both joined before returning. Each parallel task
// receives the full maximum segment size only if both fit the remaining
// archive budget. The existing disk-write gate accounts for staged bytes.
func fetchDASHSegmentBatch(ctx context.Context, fetch mediaFetcher, tasks []dashSegmentTask, budget int64) ([2]int64, error) {
	var sizes [2]int64
	if len(tasks) == 1 {
		var err error
		sizes[0], err = fetchHLSSegment(ctx, fetch, tasks[0].url, tasks[0].path, budget)
		return sizes, err
	}
	if len(tasks) != 2 || budget < 2*maxHLSSegmentBytes {
		return sizes, errors.New("DASH: недопустимый параллельный пакет")
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan dashSegmentResult, 2)
	for i, task := range tasks {
		go func(i int, task dashSegmentTask) {
			n, err := fetchHLSSegment(child, fetch, task.url, task.path, maxHLSSegmentBytes)
			results <- dashSegmentResult{index: i, size: n, err: err}
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
