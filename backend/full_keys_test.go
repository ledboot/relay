package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestFullKeysAdminOnly(t *testing.T) {
	a, _, _ := fixture(t)
	r := httptest.NewRequest("POST", "/api/admin/keys", strings.NewReader(`{"service":"openai","count":1}`))
	r.Header.Set("Authorization", "Bearer test-admin")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	var created struct {
		Keys []string `json:"keys"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	full := created.Keys[0]
	var stored string
	if err := a.db.QueryRow("SELECT full_key FROM keys WHERE hash=?", digest(full)).Scan(&stored); err != nil || stored != full {
		t.Fatal("full key not persisted", err)
	}
	r = httptest.NewRequest("GET", "/api/admin/keys", nil)
	r.Header.Set("Authorization", "Bearer test-admin")
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	var listed []struct {
		FullKey string `json:"fullKey"`
	}
	if json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed) != 2 || listed[0].FullKey != full || listed[1].FullKey != "" {
		t.Fatal("incorrect admin list")
	}
	r = httptest.NewRequest("GET", "/api/admin/keys", nil)
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 401 || strings.Contains(w.Body.String(), full) {
		t.Fatal("unauthorized key disclosure")
	}
	for _, key := range []string{full} {
		for _, path := range []string{"/api/redeem", "/api/status"} {
			w = call(a, path, key)
			if w.Code != 200 || strings.Contains(w.Body.String(), "fullKey") || strings.Contains(w.Body.String(), key) {
				t.Fatalf("public API failed or disclosed key: %s %d", path, w.Code)
			}
		}
	}
}

func TestFullKeyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE keys(id INTEGER PRIMARY KEY,hash TEXT NOT NULL UNIQUE,hint TEXT NOT NULL,service TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'ready',verification_id TEXT NOT NULL DEFAULT '',phone TEXT NOT NULL DEFAULT '',code TEXT NOT NULL DEFAULT '',message TEXT NOT NULL DEFAULT '',ends_at TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL DEFAULT ''); INSERT INTO keys(hash,hint,service) VALUES('old-hash','old-hint','openai')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		db, err = openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var full string
		err = db.QueryRow("SELECT full_key FROM keys WHERE hash='old-hash'").Scan(&full)
		db.Close()
		if err != nil || full != "" {
			t.Fatal("legacy migration failed", err)
		}
	}
}
