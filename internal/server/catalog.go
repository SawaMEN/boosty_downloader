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

	filter := catalogFilter{viewer: viewer.ID, scope: scope, query: query, author: author, status: status}
	if scope == "shared" {
		filter.ownerNames = a.accounts.catalogOwnerNames()
	}
	selection := newCatalogSelection(page, perPage, orderBy, direction)
	// In default JSON mode a shallow page can be selected directly from
	// the locked authoritative map, copying only returned records.
	if !a.catalogSQLReads {
		if result, ok := a.store.catalogJSONPage(filter, selection); ok {
			reply(w, 200, result)
			return
		}
	}

	// SQL candidate parity and deep pages retain the existing detached
	// snapshot path. All candidates still pass the same ACL and filters.
	jobs, _ := a.store.catalogJobs(r.Context(), viewer.ID, scope, a.catalogSQLReads)
	for _, job := range jobs {
		if item, ok := filter.item(job); ok {
			selection.add(item)
		}
	}
	reply(w, 200, selection.result())
}
