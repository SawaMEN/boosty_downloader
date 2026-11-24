package server

import "errors"

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

// replaceStorageBatchLocked stages detached copies, then commits exactly one
// metadata transaction. All IDs are checked before any map is modified.
// The caller holds Storage.mu and has performed authorization and SQL parity.
func (s *Storage) replaceStorageBatchLocked(jobs []Job, imports []AuthorImport) error {
	if len(jobs) == 0 && len(imports) == 0 {
		return nil
	}
	type priorJob struct {
		value  *Job
		exists bool
	}
	type priorImport struct {
		value  *AuthorImport
		exists bool
	}
	oldJobs := make(map[string]priorJob, len(jobs))
	oldImports := make(map[string]priorImport, len(imports))
	for _, item := range jobs {
		if _, seen := oldJobs[item.ID]; seen {
			return errors.New("duplicate job in metadata batch")
		}
		previous, exists := s.jobs[item.ID]
		oldJobs[item.ID] = priorJob{previous, exists}
	}
	for _, item := range imports {
		if _, seen := oldImports[item.ID]; seen {
			return errors.New("duplicate import in metadata batch")
		}
		previous, exists := s.imports[item.ID]
		oldImports[item.ID] = priorImport{previous, exists}
	}
	for _, item := range jobs {
		owned := cloneStorageJob(item)
		s.jobs[item.ID] = &owned
	}
	for _, item := range imports {
		owned := cloneStorageImport(item)
		s.imports[item.ID] = &owned
	}
	var err error
	switch {
	case len(jobs) != 0 && len(imports) != 0:
		err = s.saveJobsImportsLocked()
	case len(jobs) != 0:
		err = s.saveJobsLocked()
	default:
		err = s.saveImportsLocked()
	}
	if err == nil {
		return nil
	}
	for id, previous := range oldJobs {
		if previous.exists {
			s.jobs[id] = previous.value
		} else {
			delete(s.jobs, id)
		}
	}
	for id, previous := range oldImports {
		if previous.exists {
			s.imports[id] = previous.value
		} else {
			delete(s.imports, id)
		}
	}
	return err
}
