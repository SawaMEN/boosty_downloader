package server

import (
	"bytes"
	"errors"
	"io"
)

// Match one of two known exact images without allocating a third full
// image. A true result denotes AFTER (also preferred when identical).
// Reject a short, long or mismatched stream and propagate read failures.
// Called on a pinned regular file opened with the existing NOFOLLOW guard.
func matchSQLiteMirror(reader io.Reader, before, after []byte) (bool, error) {
	var buf [32 << 10]byte
	offset := 0
	oldOK, newOK := true, true
	for {
		n, err := io.ReadFull(reader, buf[:])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return false, err
		}
		if oldOK {
			oldOK = offset <= len(before) && n <= len(before)-offset &&
				bytes.Equal(buf[:n], before[offset:offset+n])
		}
		if newOK {
			newOK = offset <= len(after) && n <= len(after)-offset &&
				bytes.Equal(buf[:n], after[offset:offset+n])
		}
		if !oldOK && !newOK {
			return false, errors.New("mirror diverged from transaction")
		}
		offset += n
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
	}
	if newOK && offset == len(after) {
		return true, nil
	}
	if oldOK && offset == len(before) {
		return false, nil
	}
	return false, errors.New("mirror length differs from transaction")
}
