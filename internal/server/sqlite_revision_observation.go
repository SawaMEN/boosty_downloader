package server

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// sqliteReindexUnchanged prevents a historical snapshot's indexed bytes,
// ownership, subscription or content fingerprint from changing in place.
// Current snapshots are allowed to change as archive refreshes complete.
func sqliteReindexUnchanged(ctx context.Context, tx *sql.Tx, previous sqliteIndexedRevision,
	current sqliteDiskRevision, assets map[string]sqliteDiskAsset) (bool, error) {
	if previous.archiveID != current.archiveID || previous.path != current.path ||
		previous.title != current.title || !previous.captured.Equal(current.captured) ||
		len(assets) != len(current.assets) {
		return false, nil
	}
	if previous.fingerprint.Valid != (current.fingerprint != nil) {
		return false, nil
	}
	if previous.fingerprint.Valid && previous.fingerprint.String != current.fingerprint {
		return false, nil
	}
	for _, asset := range current.assets {
		old, ok := assets[asset.path]
		if !ok || old.digest != asset.digest || old.size != asset.size ||
			old.role != asset.role {
			return false, nil
		}
	}
	return sqliteReindexObservationUnchanged(ctx, tx, previous.observation, current)
}

func sqliteReindexObservationUnchanged(ctx context.Context, tx *sql.Tx,
	id sql.NullInt64, revision sqliteDiskRevision) (bool, error) {
	sub := revision.subscription
	if !id.Valid {
		return sub == nil || sub.ObservedAt.IsZero() ||
			(sub.Source != "boosty_subscriptions" && sub.Source != "unknown"), nil
	}
	if sub == nil || sub.ObservedAt.IsZero() {
		return false, nil
	}
	var owner, author, level, levelID, observed, source string
	var paid, paused int
	err := tx.QueryRowContext(ctx, `SELECT owner_id,author_slug,COALESCE(level_name,''),
		COALESCE(level_source_id,''),paid,paused,observed_at,source
		FROM subscription_observations WHERE observation_id=?`, id.Int64).
		Scan(&owner, &author, &level, &levelID, &paid, &paused, &observed, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	at, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		return false, err
	}
	return owner == revision.owner && author == revision.author &&
		level == sub.Level && levelID == sub.LevelID && paid == sqliteBool(sub.Paid) &&
		paused == sqliteBool(sub.Paused) && source == sub.Source &&
		at.Equal(sub.ObservedAt), nil
}

// sqliteReindexObservation reuses matching subscription snapshots, creating
// only missing observations inside the same SQL transaction. These describe
// the owner's tier when checked, never the post's required access level.
func sqliteReindexObservation(ctx context.Context, tx *sql.Tx, revision sqliteDiskRevision) (any, error) {
	sub := revision.subscription
	if sub == nil || sub.ObservedAt.IsZero() ||
		(sub.Source != "boosty_subscriptions" && sub.Source != "unknown") {
		return nil, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT observation_id FROM subscription_observations
		WHERE owner_id=? AND author_slug=? AND COALESCE(level_name,'')=?
		AND COALESCE(level_source_id,'')=? AND paid=? AND paused=? AND observed_at=?
		AND source=? LIMIT 1`, revision.owner, revision.author, sub.Level, sub.LevelID,
		sqliteBool(sub.Paid), sqliteBool(sub.Paused), sqliteDate(sub.ObservedAt), sub.Source).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO subscription_observations
		(owner_id,author_slug,level_name,level_source_id,paid,paused,observed_at,source)
		VALUES(?,?,?,?,?,?,?,?)`, revision.owner, revision.author,
		sub.Level, sub.LevelID, sqliteBool(sub.Paid), sqliteBool(sub.Paused),
		sqliteDate(sub.ObservedAt), sub.Source)
	if err != nil {
		return nil, err
	}
	return res.LastInsertId()
}
