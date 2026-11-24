package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A paginated, permission-aware metadata projection. No raw Boosty payloads or
// signed CDN URLs are present. Scope "shared" never lists private records.
type catalogItem struct {
	Job
	OwnerName string `json:"ownerName,omitempty"`
}
type catalogPage struct {
	Items   []catalogItem `json:"items"`
	Total   int           `json:"total"`
	Page    int           `json:"page"`
	PerPage int           `json:"perPage"`
	Pages   int           `json:"pages"`
}

func catalogNumber(s string, fallback, low, high int) (int, bool) {
	if s == "" {
		return fallback, true
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= low && n <= high
}
func catalogDate(j Job) time.Time {
	if !j.ArchiveUpdatedAt.IsZero() {
		return j.ArchiveUpdatedAt
	}
	return j.UpdatedAt
}
func (a *App) catalog(w http.ResponseWriter, r *http.Request) {
	args := r.URL.Query()
	scope := args.Get("scope")
	if scope == "" {
		scope = "mine"
	}
	if scope != "mine" && scope != "shared" {
		fail(w, 400, "неверный раздел каталога")
		return
	}
	viewer := principal(r)
	if scope == "shared" && !a.accounts.allowed(viewer.ID, "browseShared") {
		fail(w, 403, "общие архивы отключены администратором")
		return
	}
	page, ok := catalogNumber(args.Get("page"), 1, 1, 1000000)
	if !ok {
		fail(w, 400, "неверный номер страницы")
		return
	}
	perPage, ok := catalogNumber(args.Get("perPage"), 20, 1, 100)
	if !ok {
		fail(w, 400, "неверное число элементов")
		return
	}
	query := strings.ToLower(strings.TrimSpace(args.Get("q")))
	author := strings.ToLower(strings.TrimSpace(args.Get("author")))
	if len(query) > 128 || len(author) > 128 {
		fail(w, 400, "слишком длинный запрос")
		return
	}
	status := args.Get("status")
	switch status {
	case "", "done", "partial", "failed", "queued", "running":
	default:
		fail(w, 400, "неизвестный статус")
		return
	}
	orderBy := args.Get("sort")
	if orderBy == "" {
		orderBy = "updated"
	}
	switch orderBy {
	case "updated", "created", "title", "author":
	default:
		fail(w, 400, "неизвестная сортировка")
		return
	}
	direction := args.Get("order")
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		fail(w, 400, "неверный порядок сортировки")
		return
	}

	// SQLite is only an optional candidate index. Every returned job still
	// comes from authoritative JSON and passes the existing ACL/filter logic.
	jobs, _ := a.store.catalogJobs(r.Context(), viewer.ID, scope, a.catalogSQLReads)
	selection := newCatalogSelection(page, perPage, orderBy, direction)
	ownerNames := make(map[string]string)
	for _, job := range jobs {
		if scope == "mine" {
			if job.OwnerID != viewer.ID {
				continue
			}
		} else {
			if job.OwnerID == viewer.ID || (job.State != "done" && job.State != "partial") {
				continue
			}
			if !job.SharedForAll && !contains(job.SharedWith, viewer.ID) {
				continue
			}
		}
		if author != "" && !strings.Contains(strings.ToLower(job.Author), author) {
			continue
		}
		if status != "" && job.State != status {
			continue
		}
		owner := ""
		if scope == "shared" {
			var cached bool
			owner, cached = ownerNames[job.OwnerID]
			if !cached {
				owner = a.accounts.username(job.OwnerID)
				if len(ownerNames) < 4096 {
					ownerNames[job.OwnerID] = owner
				}
			}
		}
		if query != "" && !strings.Contains(strings.ToLower(job.Title), query) &&
			!strings.Contains(strings.ToLower(job.Author), query) &&
			!(scope == "shared" && strings.Contains(strings.ToLower(owner), query)) {
			continue
		}
		if scope == "shared" {
			job.SharedWith = nil
			job.Subscription = nil
			job.OwnerID = ""
			job.Error = ""
		}
		selection.add(catalogItem{Job: job, OwnerName: owner})
	}
	reply(w, 200, selection.result())
}
