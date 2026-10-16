package main

import (
	"net/http"
	"strconv"
	"strings"
)

type adminKey struct {
	Key
	FullKey string `json:"fullKey"`
}
type keyStats struct {
	Total   int `json:"total"`
	Ready   int `json:"ready"`
	Waiting int `json:"waiting"`
	Used    int `json:"used"`
}

func (a *App) listKeys(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	paginated := q.Has("page") || q.Has("pageSize") || q.Has("search") || q.Has("state")
	page, size := 1, 20
	for name, target := range map[string]*int{"page": &page, "pageSize": &size} {
		if q.Has(name) {
			n, err := strconv.Atoi(q.Get(name))
			if err != nil || n < 1 || (name == "pageSize" && n > 100) {
				fail(w, 400, "分页参数无效")
				return
			}
			*target = n
		}
	}
	if !paginated {
		size = 500
	}
	where := " WHERE 1=1"
	args := []any{}
	if state := q.Get("state"); state != "" && state != "all" {
		switch state {
		case "ready", "allocating", "waiting", "used", "expired", "timed_out", "canceled", "refunded", "reported", "disabled":
		default:
			fail(w, 400, "状态无效")
			return
		}
		where += " AND state=?"
		args = append(args, state)
	}
	if search := strings.ToLower(strings.TrimSpace(q.Get("search"))); search != "" {
		where += " AND (instr(lower(service),?)>0 OR instr(lower(full_key),?)>0 OR instr(lower(hint),?)>0)"
		args = append(args, search, search, search)
	}
	var total int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM keys"+where, args...).Scan(&total); err != nil {
		fail(w, 500, "数据库读取失败")
		return
	}
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	var stats keyStats
	if err := a.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(state='ready'),0),COALESCE(SUM(state IN ('waiting','allocating')),0),COALESCE(SUM(state='used'),0) FROM keys").Scan(&stats.Total, &stats.Ready, &stats.Waiting, &stats.Used); err != nil {
		fail(w, 500, "统计读取失败")
		return
	}
	args = append(args, size, (page-1)*size)
	rows, err := a.db.Query("SELECT "+columns+",full_key FROM keys"+where+" ORDER BY id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		fail(w, 500, "数据库读取失败")
		return
	}
	defer rows.Close()
	items := []adminKey{}
	for rows.Next() {
		var k adminKey
		if err = rows.Scan(&k.ID, &k.Hint, &k.Service, &k.State, &k.VerificationID, &k.Phone, &k.Code, &k.Message, &k.EndsAt, &k.CreatedAt, &k.Retries, &k.FullKey); err != nil {
			fail(w, 500, "数据库读取失败")
			return
		}
		items = append(items, k)
	}
	if rows.Err() != nil {
		fail(w, 500, "数据库读取失败")
		return
	}
	if !paginated {
		reply(w, 200, items)
		return
	}
	reply(w, 200, map[string]any{"items": items, "total": total, "page": page, "pageSize": size, "stats": stats})
}
