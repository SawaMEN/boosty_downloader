package server

import (
	"math"
	"reflect"
	"time"
)

// A conservative proof of successful time.Time JSON encoding. Restrict
// the shortcut to UTC so offset formatting never needs to be reproduced.
func sqliteMetadataSafeTime(t time.Time) bool {
	return t.Location() == time.UTC && t.Year() >= 0 && t.Year() <= 9999
}

func sqliteMetadataSafeSubscription(s *CapturedSubscription) bool {
	return s == nil || sqliteMetadataSafeTime(s.ObservedAt)
}

func sqliteMetadataSafeJob(j Job) bool {
	if !sqliteMetadataSafeTime(j.CreatedAt) || !sqliteMetadataSafeTime(j.UpdatedAt) ||
		!sqliteMetadataSafeTime(j.ArchiveUpdatedAt) ||
		(j.DeletedAt != nil && !sqliteMetadataSafeTime(*j.DeletedAt)) ||
		!sqliteMetadataSafeSubscription(j.Subscription) {
		return false
	}
	tier := j.RequiredTier
	return tier == nil || (!math.IsNaN(tier.TierPrice) && !math.IsInf(tier.TierPrice, 0) &&
		!math.IsNaN(tier.SinglePurchase) && !math.IsInf(tier.SinglePurchase, 0))
}

func sqliteMetadataSafeImport(i AuthorImport) bool {
	return sqliteMetadataSafeTime(i.CreatedAt) && sqliteMetadataSafeTime(i.UpdatedAt) &&
		sqliteMetadataSafeTime(i.LastCheckedAt) && sqliteMetadataSafeSubscription(i.Subscription)
}

// A successful proof is sufficient, never necessary. All other cases
// fall back to the original JSON semantics (including omitempty and
// equivalent encodings). DeepEqual includes every field, and the schema
// audit ensures the validation above covers every fallible JSON field.
func sqliteMetadataEqualValidated(a, b any) bool {
	switch left := a.(type) {
	case Job:
		right, ok := b.(Job)
		return ok && reflect.DeepEqual(left, right) && sqliteMetadataSafeJob(left)
	case AuthorImport:
		right, ok := b.(AuthorImport)
		return ok && reflect.DeepEqual(left, right) && sqliteMetadataSafeImport(left)
	default:
		return false
	}
}
