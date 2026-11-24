package server

import (
	"sort"
	"time"
)

const (
	maxLoginPeers       = 4096
	maxSessionsPerOwner = 32
	loginFailureWindow  = 15 * time.Minute
)

func recentLoginFailures(values []time.Time, now time.Time) []time.Time {
	kept := values[:0]
	for _, value := range values {
		if now.Sub(value) < loginFailureWindow {
			kept = append(kept, value)
		}
	}
	return kept
}

// Bound unauthenticated state. Never evict a live rate-limit entry to make
// room for a new IP; fail closed until an expired entry can be removed.
func (a *Accounts) allowLoginPeerLocked(peer string, now time.Time) bool {
	if a.failed == nil {
		a.failed = make(map[string][]time.Time)
	}
	if now.Sub(a.failedSweep) >= time.Minute || len(a.failed) >= maxLoginPeers {
		for key, values := range a.failed {
			kept := recentLoginFailures(values, now)
			if len(kept) == 0 {
				delete(a.failed, key)
			} else {
				a.failed[key] = kept
			}
		}
		a.failedSweep = now
	}
	values, exists := a.failed[peer]
	if !exists && len(a.failed) >= maxLoginPeers {
		return false
	}
	a.failed[peer] = recentLoginFailures(values, now)
	return len(a.failed[peer]) < 10
}

// Keep only valid unexpired sessions. Reserve room for the new token by
// expiring oldest sessions of that owner, leaving other owners untouched
// except for their already expired/orphaned sessions.
func boundedLoginSessions(before map[string]LoginSession, accounts map[string]Account, owner string, now time.Time) map[string]LoginSession {
	next := make(map[string]LoginSession, len(before)+1)
	var owned []string
	for key, session := range before {
		if !session.Expires.After(now) {
			continue
		}
		if _, ok := accounts[session.UserID]; !ok {
			continue
		}
		next[key] = session
		if session.UserID == owner {
			owned = append(owned, key)
		}
	}
	sort.Slice(owned, func(i, j int) bool {
		a, b := next[owned[i]].Expires, next[owned[j]].Expires
		if a.Equal(b) {
			return owned[i] < owned[j]
		}
		return a.Before(b)
	})
	for len(owned) >= maxSessionsPerOwner {
		delete(next, owned[0])
		owned = owned[1:]
	}
	return next
}
