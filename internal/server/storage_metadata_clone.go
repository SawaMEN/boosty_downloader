package server

import "slices"

// Values crossing the Storage boundary must not retain writable references
// into the canonical metadata maps. Call while holding Storage.mu for values
// originating from those maps. Callers must not mutate an input concurrently.
func cloneStorageJob(job Job) Job {
	job.SharedWith = slices.Clone(job.SharedWith)
	if job.DeletedAt != nil {
		value := *job.DeletedAt
		job.DeletedAt = &value
	}
	if job.Subscription != nil {
		value := *job.Subscription
		job.Subscription = &value
	}
	if job.RequiredTier != nil {
		value := *job.RequiredTier
		job.RequiredTier = &value
	}
	return job
}

func cloneStorageImport(item AuthorImport) AuthorImport {
	if item.Subscription != nil {
		value := *item.Subscription
		item.Subscription = &value
	}
	return item
}
