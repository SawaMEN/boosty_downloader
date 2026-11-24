package server

// Retain at most 1 MiB of idle copy buffers per process. Never block a
// download waiting for a buffer: concurrent copies may allocate their own.
// The active buffer belongs exclusively to its caller until release.
const guardedCopyPoolCapacity = 4

type guardedCopyBuffer [guardedCopyChunkBytes]byte

var guardedCopyBuffers = make(chan *guardedCopyBuffer, guardedCopyPoolCapacity)

func acquireGuardedCopyBuffer() *guardedCopyBuffer {
	select {
	case buf := <-guardedCopyBuffers:
		return buf
	default:
		return new(guardedCopyBuffer)
	}
}

func releaseGuardedCopyBuffer(buf *guardedCopyBuffer) {
	// Different owners can use this pool. Erase all bytes, including the
	// unused suffix of the last read, before another copy can acquire it.
	clear(buf[:])
	select {
	case guardedCopyBuffers <- buf:
	default:
	}
}
