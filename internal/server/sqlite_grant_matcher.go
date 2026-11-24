package server

import "errors"

const sqliteSmallGrantLimit = 8

func validateSQLiteGrants(grants []string) error {
	if len(grants) <= sqliteSmallGrantLimit {
		for i, member := range grants {
			if !safeArchivePart(member) {
				return errors.New("invalid or duplicate archive grant")
			}
			for _, earlier := range grants[:i] {
				if member == earlier {
					return errors.New("invalid or duplicate archive grant")
				}
			}
		}
		return nil
	}
	seen := make(map[string]bool, len(grants))
	for _, member := range grants {
		if !safeArchivePart(member) || seen[member] {
			return errors.New("invalid or duplicate archive grant")
		}
		seen[member] = true
	}
	return nil
}

// Only for a validated payload: grants are unique and safe archive parts.
// Retain the original slice read-only, and consume each SQL grant exactly
// once. For larger ACLs the map keeps matching linear rather than quadratic.
type sqliteGrantMatcher struct {
	small     []string
	remaining map[string]bool
	consumed  uint8
	count     int
}

func newSQLiteGrantMatcher(grants []string) sqliteGrantMatcher {
	m := sqliteGrantMatcher{count: len(grants)}
	if len(grants) <= sqliteSmallGrantLimit {
		m.small = grants
		return m
	}
	m.remaining = make(map[string]bool, len(grants))
	for _, member := range grants {
		m.remaining[member] = true
	}
	return m
}

func (m *sqliteGrantMatcher) consume(member string) bool {
	if m.remaining != nil {
		if !m.remaining[member] {
			return false
		}
		delete(m.remaining, member)
		m.count--
		return true
	}
	for i, expected := range m.small {
		if member != expected {
			continue
		}
		bit := uint8(1) << i
		if m.consumed&bit != 0 {
			return false
		}
		m.consumed |= bit
		m.count--
		return true
	}
	return false
}

func (m *sqliteGrantMatcher) complete() bool {
	return m.count == 0
}
