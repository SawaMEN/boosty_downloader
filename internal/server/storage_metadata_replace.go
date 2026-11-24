package server

// replaceStorageJobLocked is the common single-job persistence boundary.
// Call with Storage.mu held, after the mutation guard and operation-specific
// checks. The previous entry is never modified in place. On failure restore
// the exact previous pointer, including timestamps and all nested fields.
// saveJobsLocked accounts for the attempt in the metadata mutation epoch.
func (s *Storage) replaceStorageJobLocked(next Job) error {
	previous, exists := s.jobs[next.ID]
	owned := cloneStorageJob(next)
	s.jobs[next.ID] = &owned
	if err := s.saveJobsLocked(); err != nil {
		if exists {
			s.jobs[next.ID] = previous
		} else {
			delete(s.jobs, next.ID)
		}
		return err
	}
	return nil
}
