package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestKeyPagination(t *testing.T) {
	a, _, _ := fixture(t)
	for i := 0; i < 525; i++ {
		state := "ready"
		if i%2 == 0 {
			state = "used"
		}
		if _, err := a.db.Exec("INSERT INTO keys(hash,hint,full_key,service,state) VALUES(?,?,?,?,?)", fmt.Sprint(i), fmt.Sprint(i), fmt.Sprintf("RELAY-%03d", i), "openai", state); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query                      string
		status, total, page, count int
	}{
		{"?page=1&pageSize=20", 200, 526, 1, 20},
		{"?page=27&pageSize=20", 200, 526, 27, 6},
		{"?page=999&pageSize=20", 200, 526, 27, 6},
		{"?page=1&pageSize=50&state=used", 200, 263, 1, 50},
		{"?page=8&search=relay-000", 200, 1, 1, 1},
		{"?page=1&search=missing", 200, 0, 1, 0},
		{"?page=1&search=%25", 200, 0, 1, 0},
		{"?page=0", 400, 0, 0, 0}, {"?page=abc", 400, 0, 0, 0}, {"?pageSize=101", 400, 0, 0, 0}, {"?state=invalid", 400, 0, 0, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/admin/keys"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer test-admin")
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status %d", w.Code)
			}
			if tc.status != 200 {
				return
			}
			var result struct {
				Items       []adminKey
				Total, Page int
				Stats       keyStats
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Total != tc.total || result.Page != tc.page || len(result.Items) != tc.count {
				t.Fatalf("incorrect pagination: total=%d page=%d count=%d", result.Total, result.Page, len(result.Items))
			}
			if result.Stats.Total != 526 || result.Stats.Ready != 263 || result.Stats.Used != 263 {
				t.Fatalf("incorrect global stats: %+v", result.Stats)
			}
			for i := 1; i < len(result.Items); i++ {
				if result.Items[i-1].ID <= result.Items[i].ID {
					t.Fatal("incorrect ordering")
				}
			}
		})
	}
}
