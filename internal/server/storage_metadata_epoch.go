package server

// invalidateMetadataEpochLocked conservatively invalidates a process-local
// metadata generation before a persistence attempt. The caller owns mu
// (or is initializing an unpublished Storage). Never roll back this epoch:
// a failed fsync can conceal a successful rename, and even a clean rollback
// must not resurrect a previously cached proof.
//
// This is diagnostic bookkeeping only. It does NOT certify that all direct
// map mutations are tracked and MUST NOT be used to bypass SQL parity.
// MaxUint64 is a permanent invalid sentinel rather than wrapping to zero.
func (s *Storage) invalidateMetadataEpochLocked() {
	if s.metadataMutationEpoch != ^uint64(0) {
		s.metadataMutationEpoch++
	}
}
