package server

import (
	"context"
	"errors"
	"time"
)

// catalogIDs is a strictly metadata-only, owner-scoped SQL index query. Never
// use a SQL grant, payload or deletion marker as authorization by itself:
// authoritative JSON must independently confirm the complete set below.
func (m *sqliteMetadata) catalogIDs(ctx context.Context, viewer, scope string, maxRows int) ([]string, error) {
	if !safeArchivePart(viewer) {
		return nil, errors.New("invalid catalog viewer")
	}
	var query string
	switch scope {
	case "mine":
		query = `SELECT archive_id FROM owner_archives
			WHERE owner_id=? AND deleted_at IS NULL ORDER BY archive_id`
	case "shared":
		query = `SELECT a.archive_id FROM owner_archives a
			WHERE a.owner_id<>? AND a.deleted_at IS NULL
			  AND a.job_state IN ('done','partial')
			  AND (a.shared_for_all=1 OR EXISTS (
				SELECT 1 FROM archive_grants g
				WHERE g.archive_id=a.archive_id AND g.grantee_owner_id=?
			  )) ORDER BY a.archive_id`
	default:
		return nil, errors.New("invalid catalog scope")
	}
	var rowsArgs []any
	if scope == "mine" {
		rowsArgs = []any{viewer}
	} else {
		rowsArgs = []any{viewer, viewer}
	}
	rows, err := m.db.QueryContext(ctx, query, rowsArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !safeArchivePart(id) || len(ids) >= maxRows {
			return nil, errors.New("invalid or unbounded SQLite catalog index")
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// catalogJobs is the first opt-in online SQL-backed catalog read path. It only
// narrows the candidate inventory; JSON remains the authority for every owner,
// grant and content field. An out-of-sync, forged or unavailable SQL inventory
// falls back to the complete JSON snapshot rather than hiding items or leaking
// private data. This deliberately trades some speed for a safe migration path.
func (s *Storage) catalogJobs(ctx context.Context, viewer, scope string, sqlReads bool) ([]Job, bool) {
	authoritative := s.all()
	if !sqlReads || s.shadow == nil {
		return authoritative, false
	}
	status := s.sqliteShadowStatus()
	if status.Pending || status.LastError != "" || status.LastSuccess.IsZero() {
		return authoritative, false
	}
	queryCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	ids, err := s.shadow.meta.catalogIDs(queryCtx, viewer, scope, len(authoritative))
	if err != nil {
		return authoritative, false
	}
	expected := make(map[string]Job)
	for _, job := range authoritative {
		if catalogJobVisible(job, viewer, scope) {
			expected[job.ID] = job
		}
	}
	if len(ids) != len(expected) {
		return authoritative, false
	}
	narrowed := make([]Job, 0, len(ids))
	for _, id := range ids {
		job, ok := expected[id]
		if !ok {
			return authoritative, false
		}
		delete(expected, id)
		narrowed = append(narrowed, job)
	}
	// A concurrent JSON mutation invalidates the SQL list used here.
	after := s.sqliteShadowStatus()
	if after.Pending || after.LastError != "" || after.LastSuccess.IsZero() || !after.LastSuccess.Equal(status.LastSuccess) {
		return authoritative, false
	}
	return narrowed, true
}

func catalogJobVisible(job Job, viewer, scope string) bool {
	if job.DeletedAt != nil {
		return false
	}
	if scope == "mine" {
		return job.OwnerID == viewer
	}
	return scope == "shared" && job.OwnerID != viewer &&
		(job.State == "done" || job.State == "partial") &&
		(job.SharedForAll || contains(job.SharedWith, viewer))
}
