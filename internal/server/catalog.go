package server

import (
	"net/http"
	"sort"
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
	Items []catalogItem `json:"items"`
	Total int `json:"total"`
	Page int `json:"page"`
	PerPage int `json:"perPage"`
	Pages int `json:"pages"`
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

	items := make([]catalogItem, 0)
	for _, job := range a.store.all() {
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
			owner = a.accounts.username(job.OwnerID)
		}
		if query != "" && !strings.Contains(strings.ToLower(job.Title), query) &&
			!strings.Contains(strings.ToLower(job.Author), query) &&
			!(scope == "shared" && strings.Contains(strings.ToLower(owner), query)) {
			continue
		}
		if scope == "shared" {
			job.SharedWith = nil
			job.OwnerID = ""
			job.Error = ""
		}
		items = append(items, catalogItem{Job: job, OwnerName: owner})
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i], items[j]
		comparison := 0
		switch orderBy {
		case "created":
			if left.CreatedAt.Before(right.CreatedAt) {
				comparison = -1
			} else if left.CreatedAt.After(right.CreatedAt) {
				comparison = 1
			}
		case "title":
			comparison = strings.Compare(strings.ToLower(left.Title), strings.ToLower(right.Title))
		case "author":
			comparison = strings.Compare(strings.ToLower(left.Author), strings.ToLower(right.Author))
		default:
			if catalogDate(left.Job).Before(catalogDate(right.Job)) {
				comparison = -1
			} else if catalogDate(left.Job).After(catalogDate(right.Job)) {
				comparison = 1
			}
		}
		if comparison == 0 {
			return left.ID < right.ID
		}
		if direction == "asc" {
			return comparison < 0
		}
		return comparison > 0
	})
	total := len(items)
	pages := (total + perPage - 1) / perPage
	start := total
	if page - 1 <= total / perPage {
		start = (page - 1) * perPage
		if start > total {
			start = total
		}
	}
	end := start + perPage
	if end > total {
		end = total
	}
	reply(w, 200, catalogPage{Items: items[start:end], Total: total, Page: page, PerPage: perPage, Pages: pages})
}
