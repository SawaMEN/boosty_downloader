package server

import (
	"context"
	"database/sql"
)

// A generation probe is connection-local. It is only a diagnostic building
// block, not authorization to skip parity checks. In particular, SQLite
// cannot detect in-memory map mutation or direct filesystem corruption.
// Compare samples only on the same pinned connection; never on *sql.DB.
type sqliteGenerationProbe struct {
	DataVersion   int64
	TotalChanges  int64
	SchemaVersion int64
}

func sqliteReadGenerationProbe(ctx context.Context, conn *sql.Conn) (sqliteGenerationProbe, error) {
	var probe sqliteGenerationProbe
	if err := conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&probe.DataVersion); err != nil {
		return sqliteGenerationProbe{}, err
	}
	if err := conn.QueryRowContext(ctx, "SELECT total_changes()").Scan(&probe.TotalChanges); err != nil {
		return sqliteGenerationProbe{}, err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&probe.SchemaVersion); err != nil {
		return sqliteGenerationProbe{}, err
	}
	// These statements do not constitute an atomic snapshot. The probe is
	// deliberately not used to authorize production reads or mutations.
	return probe, nil
}
