package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// The JSON archive record is the commit decision: BEFORE => put staged
// folders back, AFTER => finish removing stages. The durable journal is
// created before the first directory rename. Pending journals block startup
// if neither generation matches; never guess whether an archive was purged.
const purgeJournalName = ".archive-purge-transaction.json"

type purgeMove struct {
	Kind   string `json:"kind"`
	Owner  string `json:"owner"`
	Author string `json:"author"`
	Post   string `json:"post"`
	Slot   string `json:"slot"`
	Device uint64 `json:"device,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
}

type purgeJournal struct {
	Version    int         `json:"version"`
	BeforeHash string      `json:"beforeHash"`
	AfterHash  string      `json:"afterHash"`
	Moves      []purgeMove `json:"moves"`
}

func purgeValidHex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func purgeValidSlot(value string) bool {
	if len(value) != 24 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func purgeMovePaths(root string, move purgeMove) (string, string, error) {
	if (move.Kind != "library" && move.Kind != "revisions") ||
		!safeArchivePart(move.Owner) || !safeArchivePart(move.Author) ||
		!safeArchivePart(move.Post) || !purgeValidSlot(move.Slot) {
		return "", "", errors.New("invalid archive purge journal path component")
	}
	src := filepath.Join(root, move.Kind, move.Owner, move.Author, move.Post)
	dst := filepath.Join(root, "trash", move.Slot)
	return src, dst, nil
}

// Validate each path component using lstat, not Stat. The journal may never
// walk through a symlink even when the final file is a regular directory.
// A missing component is fine only for absent archive/revision folders.
func purgeSafePath(root, path string) (os.FileInfo, error) {
	base, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("archive purge path escapes data directory")
	}
	baseInfo, err := os.Lstat(base)
	if err != nil || !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe archive purge data root")
	}
	current := base
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe purge directory: %s", current)
		}
		if i == len(parts)-1 {
			return info, nil
		}
	}
	return nil, errors.New("archive purge path has no components")
}

// Plan only actual folders, and pin random, non-reusable destination slots.
// The journal never accepts an absolute or caller-selected destination path.
func planPurgeMoves(root string, folders map[string]bool) ([]purgeMove, error) {
	trash := filepath.Join(root, "trash")
	if err := os.MkdirAll(trash, 0700); err != nil {
		return nil, err
	}
	if info, err := purgeSafePath(root, trash); err != nil || info == nil {
		return nil, errors.New("unsafe staging trash directory")
	}
	if err := syncMetadataDirectory(root); err != nil {
		return nil, err
	}
	ordered := make([]string, 0, len(folders))
	for path := range folders {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	if len(ordered) > 100000 {
		return nil, errors.New("unbounded physical archive purge")
	}
	moves := make([]purgeMove, 0, len(ordered))
	for _, path := range ordered {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 4 {
			return nil, errors.New("invalid purge source layout")
		}
		m := purgeMove{Kind: parts[0], Owner: parts[1], Author: parts[2], Post: parts[3]}
		src, _, err := purgeMovePaths(root, purgeMove{Kind: m.Kind, Owner: m.Owner, Author: m.Author, Post: m.Post, Slot: strings.Repeat("0", 24)})
		if err != nil || src != path {
			return nil, errors.New("unsafe purge source identity")
		}
		info, err := purgeSafePath(root, src)
		if err != nil {
			return nil, err
		}
		if info == nil {
			continue
		}
		var token [12]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, err
		}
		m.Slot = hex.EncodeToString(token[:])
		m.Device, m.Inode, err = revisionRestoreIdentity(root, src)
		if err != nil {
			return nil, err
		}
		_, dst, _ := purgeMovePaths(root, m)
		if existing, err := purgeSafePath(root, dst); err != nil || existing != nil {
			return nil, errors.New("archive purge stage destination already exists or is unsafe")
		}
		moves = append(moves, m)
	}
	return moves, nil
}

func writePurgeJournal(root string, journal purgeJournal) error {
	if (journal.Version != 1 && journal.Version != 2) || !purgeValidHex(journal.BeforeHash) ||
		!purgeValidHex(journal.AfterHash) || journal.BeforeHash == journal.AfterHash {
		return errors.New("invalid archive purge journal generation")
	}
	path := filepath.Join(root, purgeJournalName)
	if _, err := os.Lstat(path); err == nil {
		return errors.New("archive purge journal already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writeSyncedMetadata(root, purgeJournalName, data)
}

func purgeStageMove(root string, move purgeMove) error {
	src, dst, err := purgeMovePaths(root, move)
	if err != nil {
		return err
	}
	if info, err := purgeSafePath(root, src); err != nil || info == nil {
		return errors.New("archive disappeared before staged purge")
	}
	if move.Inode != 0 {
		if _, err := revisionRestoreDirectory(root, src, "", move.Author, move.Post, move.Device, move.Inode, false); err != nil {
			return err
		}
	}
	if info, err := purgeSafePath(root, dst); err != nil || info != nil {
		return errors.New("archive stage was already occupied")
	}
	if err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := syncMetadataDirectory(filepath.Dir(src)); err != nil {
		return err
	}
	return syncMetadataDirectory(filepath.Dir(dst))
}

// RecoverArchivePurgeLocked is called after metadata and SQL-primary journals
// are resolved, before archive readers, account migrations or worker startup.
// The caller owns the data-directory process lock.
func recoverArchivePurgeLocked(root string) error {
	path := filepath.Join(root, purgeJournalName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 32<<20 {
		return errors.New("unsafe archive purge transaction journal")
	}
	raw, err := videoPublicationReadFile(root, purgeJournalName, 32<<20)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var journal purgeJournal
	if err := dec.Decode(&journal); err != nil {
		return fmt.Errorf("invalid archive purge journal: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("archive purge journal has trailing data")
	}
	if (journal.Version != 1 && journal.Version != 2) || !purgeValidHex(journal.BeforeHash) ||
		!purgeValidHex(journal.AfterHash) || journal.BeforeHash == journal.AfterHash ||
		len(journal.Moves) > 100000 {
		return errors.New("invalid archive purge journal version or generation")
	}
	jobsPath := filepath.Join(root, "jobs.json")
	jobsInfo, err := os.Lstat(jobsPath)
	if err != nil || !jobsInfo.Mode().IsRegular() {
		return errors.New("archive purge recovery has no safe jobs.json")
	}
	jobsRaw, err := os.ReadFile(jobsPath)
	if err != nil {
		return err
	}
	generation := metadataDigest(jobsRaw)
	before := generation == journal.BeforeHash
	committed := generation == journal.AfterHash
	if !before && !committed {
		return errors.New("archive purge recovery found unknown metadata generation")
	}
	type pending struct {
		src    string
		dst    string
		staged bool
	}
	pendingMoves := make([]pending, 0, len(journal.Moves))
	seenSrc, seenDst := make(map[string]bool), make(map[string]bool)
	for _, move := range journal.Moves {
		src, dst, err := purgeMovePaths(root, move)
		if err != nil || seenSrc[src] || seenDst[dst] {
			return errors.New("invalid or duplicate archive purge journal entry")
		}
		seenSrc[src], seenDst[dst] = true, true
		srcInfo, err := purgeSafePath(root, src)
		if err != nil {
			return err
		}
		dstInfo, err := purgeSafePath(root, dst)
		if err != nil {
			return err
		}
		if journal.Version == 2 {
			if move.Device == 0 || move.Inode == 0 {
				return errors.New("missing pinned purge directory identity")
			}
			for _, candidate := range []string{src, dst} {
				if _, err := revisionRestoreDirectory(root, candidate, "", move.Author, move.Post, move.Device, move.Inode, false); err != nil {
					return err
				}
			}
		}
		if before && (srcInfo == nil) == (dstInfo == nil) {
			return errors.New("uncommitted purge cannot identify the original archive folder")
		}
		if committed && srcInfo != nil {
			return errors.New("committed purge unexpectedly has a live archive folder")
		}
		pendingMoves = append(pendingMoves, pending{src: src, dst: dst, staged: dstInfo != nil})
	}
	// Two-pass validation ensures no unrelated path is changed when a later
	// journal entry is invalid. An interrupted rollback remains idempotent.
	if before {
		for i := len(pendingMoves) - 1; i >= 0; i-- {
			m := pendingMoves[i]
			if !m.staged {
				continue
			}
			if parent, err := purgeSafePath(root, filepath.Dir(m.src)); err != nil || parent == nil {
				return errors.New("original archive parent vanished before purge rollback")
			}
			if err := unix.Renameat2(unix.AT_FDCWD, m.dst, unix.AT_FDCWD, m.src, unix.RENAME_NOREPLACE); err != nil {
				return err
			}
			if err := syncMetadataDirectory(filepath.Dir(m.src)); err != nil {
				return err
			}
			if err := syncMetadataDirectory(filepath.Dir(m.dst)); err != nil {
				return err
			}
		}
	} else {
		for _, m := range pendingMoves {
			if m.staged {
				if err := os.RemoveAll(m.dst); err != nil {
					return err
				}
			}
		}
		if err := syncMetadataDirectory(filepath.Join(root, "trash")); err != nil {
			return err
		}
	}
	// A restart must not accept an old journal over a newly changed job file.
	finalJobs, err := os.ReadFile(jobsPath)
	if err != nil || metadataDigest(finalJobs) != generation {
		return errors.New("archive purge metadata changed during recovery")
	}
	finalJournal, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(finalJournal, raw) {
		return errors.New("archive purge journal changed during recovery")
	}
	return removeSyncedMetadata(root, purgeJournalName)
}
