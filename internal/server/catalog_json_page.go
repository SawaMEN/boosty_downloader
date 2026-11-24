package server

import "strings"

type catalogFilter struct {
	viewer, scope, query, author, status string
	ownerNames                           map[string]string
}

func (a *Accounts) catalogOwnerNames() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make(map[string]string, len(a.data.Accounts))
	for id, account := range a.data.Accounts {
		names[id] = account.Username
	}
	return names
}

// This function never mutates references contained in Job. Shared fields
// are removed from the local value only; the canonical record is untouched.
func (f catalogFilter) item(job Job) (catalogItem, bool) {
	if !catalogJobVisible(job, f.viewer, f.scope) {
		return catalogItem{}, false
	}
	author := strings.ToLower(job.Author)
	if f.author != "" && !strings.Contains(author, f.author) {
		return catalogItem{}, false
	}
	if f.status != "" && job.State != f.status {
		return catalogItem{}, false
	}
	owner := ""
	if f.scope == "shared" {
		owner = f.ownerNames[job.OwnerID]
	}
	if f.query != "" && !strings.Contains(strings.ToLower(job.Title), f.query) &&
		!strings.Contains(author, f.query) &&
		!(f.scope == "shared" && strings.Contains(strings.ToLower(owner), f.query)) {
		return catalogItem{}, false
	}
	if f.scope == "shared" {
		job.SharedWith = nil
		job.Subscription = nil
		job.OwnerID = ""
		job.Error = ""
	}
	return catalogItem{Job: job, OwnerName: owner}, true
}

// JSON-only shallow selection. The map and any borrowed references stay
// under Storage.mu throughout scanning/ranking. Only detached result rows
// leave this method; neither callbacks nor account locks run here.
// SQL-primary and deep pages deliberately retain the existing read path.
func (s *Storage) catalogJSONPage(filter catalogFilter, selection *catalogSelection) (catalogPage, bool) {
	if selection.limit == 0 {
		return catalogPage{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.primary != nil {
		return catalogPage{}, false
	}
	if s.primaryFatal != nil {
		return selection.result(), true
	}
	for _, job := range s.jobs {
		if item, ok := filter.item(*job); ok {
			selection.add(item)
		}
	}
	page := selection.result()
	for i := range page.Items {
		page.Items[i].Job = cloneStorageJob(page.Items[i].Job)
	}
	// Clear borrowed references even if a future caller reuses selection.
	clear(selection.rank.values)
	selection.rank.values = nil
	return page, true
}
