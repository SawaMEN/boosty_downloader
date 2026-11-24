package server

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// The caller already decoded the before image and validated the proposed
// typed after image. Reuse those values to avoid decoding a second complete
// raw-row map and serializing every unchanged row individually. This is only
// an optimization: canonical before bytes and exact reconstructed after bytes
// are required before selecting the existing v2 journal format.
func sqlitePrimaryTypedDelta[T any](img metadataImage, before, after map[string]T) (sqlitePrimaryDeltaImage, bool) {
	if !img.Existed {
		return sqlitePrimaryDeltaImage{}, false
	}
	rows, ok := sqlitePrimaryCanonicalRows(img.Before)
	if !ok || len(rows) != len(before) {
		return sqlitePrimaryDeltaImage{}, false
	}
	out := sqlitePrimaryDeltaImage{
		OldHash: img.OldHash,
		NewHash: img.NewHash,
		Rows:    make(map[string]sqlitePrimaryRowDelta),
	}
	if bytes.Equal(img.Before, img.After) {
		return out, true
	}
	for id, old := range before {
		raw, exists := rows[id]
		if !exists {
			return sqlitePrimaryDeltaImage{}, false
		}
		next, keep := after[id]
		if !keep {
			out.Rows[id] = sqlitePrimaryRowDelta{Before: raw}
			delete(rows, id)
			continue
		}
		if reflect.DeepEqual(old, next) {
			continue
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return sqlitePrimaryDeltaImage{}, false
		}
		if !bytes.Equal(raw, encoded) {
			out.Rows[id] = sqlitePrimaryRowDelta{Before: raw, After: encoded}
			rows[id] = encoded
		}
	}
	for id, next := range after {
		if _, exists := before[id]; exists {
			continue
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return sqlitePrimaryDeltaImage{}, false
		}
		out.Rows[id] = sqlitePrimaryRowDelta{After: encoded}
		rows[id] = encoded
	}
	// A legacy row may have unknown fields or a different representation
	// despite typed equality. Never infer canonical equivalence from types.
	reconstructed, err := json.MarshalIndent(rows, "", "  ")
	if err != nil || !bytes.Equal(reconstructed, img.After) {
		return sqlitePrimaryDeltaImage{}, false
	}
	return out, true
}

func encodeSQLitePrimaryTypedJournal(journal sqlitePrimaryTransaction,
	beforeJobs map[string]Job, beforeImports map[string]AuthorImport,
	jobs map[string]Job, imports map[string]AuthorImport, limit int) ([]byte, error) {
	jobDelta, jobsOK := sqlitePrimaryTypedDelta(journal.Jobs, beforeJobs, jobs)
	importDelta, importsOK := sqlitePrimaryTypedDelta(journal.Imports, beforeImports, imports)
	if jobsOK && importsOK {
		raw, err := json.Marshal(sqlitePrimaryDeltaJournal{
			Version: sqlitePrimaryDeltaVersion, Jobs: jobDelta, Imports: importDelta,
		})
		if err != nil {
			return nil, err
		}
		if len(raw) <= limit {
			return raw, nil
		}
	}
	// Preserve the exact existing generic delta and v1 fallback rules.
	return encodeSQLitePrimaryBestJournal(journal, limit)
}
