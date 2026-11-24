package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const sqlitePrimaryDeltaVersion = 2

type sqlitePrimaryRowDelta struct {
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

type sqlitePrimaryDeltaImage struct {
	OldHash string                           `json:"oldHash"`
	NewHash string                           `json:"newHash"`
	Rows    map[string]sqlitePrimaryRowDelta `json:"rows"`
}

type sqlitePrimaryDeltaJournal struct {
	Version int                     `json:"version"`
	Jobs    sqlitePrimaryDeltaImage `json:"jobs"`
	Imports sqlitePrimaryDeltaImage `json:"imports"`
}

// A delta is allowed only for byte-exact round-trippable JSON objects.
// Legacy formatting falls back to the full v1 recovery images.
func sqlitePrimaryCanonicalRows(raw []byte) (map[string]json.RawMessage, bool) {
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil || rows == nil {
		return nil, false
	}
	encoded, err := json.MarshalIndent(rows, "", "  ")
	return rows, err == nil && bytes.Equal(encoded, raw)
}

func sqlitePrimaryMakeDelta(img metadataImage) (sqlitePrimaryDeltaImage, bool) {
	before, ok := sqlitePrimaryCanonicalRows(img.Before)
	if !ok || !img.Existed {
		return sqlitePrimaryDeltaImage{}, false
	}
	after, ok := sqlitePrimaryCanonicalRows(img.After)
	if !ok {
		return sqlitePrimaryDeltaImage{}, false
	}
	out := sqlitePrimaryDeltaImage{
		OldHash: img.OldHash,
		NewHash: img.NewHash,
		Rows:    make(map[string]sqlitePrimaryRowDelta),
	}
	for id, old := range before {
		next := after[id]
		if !bytes.Equal(old, next) {
			out.Rows[id] = sqlitePrimaryRowDelta{Before: old, After: next}
		}
	}
	for id, next := range after {
		if _, exists := before[id]; !exists {
			out.Rows[id] = sqlitePrimaryRowDelta{After: next}
		}
	}
	return out, true
}

// The on-disk mirror must be one of the exact hashed generations. Rebuild
// BOTH images independently and verify their hashes before recovery can
// consult SQL or publish any file. A missing row represents a deletion.
func sqlitePrimaryExpandDelta(raw []byte, delta sqlitePrimaryDeltaImage) (metadataImage, error) {
	hash := metadataDigest(raw)
	if hash != delta.OldHash && hash != delta.NewHash {
		return metadataImage{}, errors.New("SQL delta target diverged from both journal generations")
	}
	rows, ok := sqlitePrimaryCanonicalRows(raw)
	if !ok || delta.Rows == nil {
		return metadataImage{}, errors.New("invalid SQL delta target or row inventory")
	}
	before := make(map[string]json.RawMessage, len(rows))
	after := make(map[string]json.RawMessage, len(rows))
	for id, value := range rows {
		before[id], after[id] = value, value
	}
	for id, change := range delta.Rows {
		if !safeArchivePart(id) || bytes.Equal(change.Before, change.After) {
			return metadataImage{}, errors.New("invalid SQL delta row")
		}
		for _, side := range []struct {
			rows  map[string]json.RawMessage
			value json.RawMessage
		}{
			{before, change.Before},
			{after, change.After},
		} {
			if len(side.value) == 0 {
				delete(side.rows, id)
			} else {
				side.rows[id] = side.value
			}
		}
	}
	oldRaw, err := json.MarshalIndent(before, "", "  ")
	if err != nil {
		return metadataImage{}, err
	}
	newRaw, err := json.MarshalIndent(after, "", "  ")
	if err != nil {
		return metadataImage{}, err
	}
	img := metadataImage{Existed: true, Before: oldRaw, After: newRaw, OldHash: delta.OldHash, NewHash: delta.NewHash}
	if err := validateMetadataImage(img); err != nil {
		return metadataImage{}, err
	}
	return img, nil
}

// Prefer delta only if both images can be reconstructed byte-for-byte.
// The existing v1 decoder remains supported; the format version is explicit.
func encodeSQLitePrimaryBestJournal(journal sqlitePrimaryTransaction, limit int) ([]byte, error) {
	jobs, jobsOK := sqlitePrimaryMakeDelta(journal.Jobs)
	imports, importsOK := sqlitePrimaryMakeDelta(journal.Imports)
	if jobsOK && importsOK {
		raw, err := json.Marshal(sqlitePrimaryDeltaJournal{Version: sqlitePrimaryDeltaVersion, Jobs: jobs, Imports: imports})
		if err != nil {
			return nil, err
		}
		if len(raw) <= limit {
			return raw, nil
		}
	}
	return encodeSQLitePrimaryJournal(journal, limit)
}

func decodeSQLitePrimaryJournal(root string, raw []byte) (sqlitePrimaryTransaction, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return sqlitePrimaryTransaction{}, err
	}
	switch header.Version {
	case sqlitePrimaryTransactionVersion:
		var journal sqlitePrimaryTransaction
		err := json.Unmarshal(raw, &journal)
		return journal, err
	case sqlitePrimaryDeltaVersion:
		var delta sqlitePrimaryDeltaJournal
		if err := json.Unmarshal(raw, &delta); err != nil {
			return sqlitePrimaryTransaction{}, err
		}
		var journal sqlitePrimaryTransaction
		journal.Version = sqlitePrimaryTransactionVersion
		for _, target := range []struct {
			name  string
			delta sqlitePrimaryDeltaImage
			out   *metadataImage
		}{
			{"jobs.json", delta.Jobs, &journal.Jobs},
			{"imports.json", delta.Imports, &journal.Imports},
		} {
			path := filepath.Join(root, target.name)
			info, err := os.Lstat(path)
			if err != nil {
				return sqlitePrimaryTransaction{}, err
			}
			if !info.Mode().IsRegular() {
				return sqlitePrimaryTransaction{}, errors.New("unsafe SQL delta mirror")
			}
			current, err := os.ReadFile(path)
			if err != nil {
				return sqlitePrimaryTransaction{}, err
			}
			img, err := sqlitePrimaryExpandDelta(current, target.delta)
			if err != nil {
				return sqlitePrimaryTransaction{}, fmt.Errorf("%s: %w", target.name, err)
			}
			*target.out = img
		}
		return journal, nil
	default:
		return sqlitePrimaryTransaction{}, errors.New("unsupported SQL primary journal version")
	}
}
