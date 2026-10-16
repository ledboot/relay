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
	// The pre-rental schema: no full_key, no retries, and the reservation column is still
	// named verification_id with the old partial unique index.
	_, err = db.Exec(`CREATE TABLE keys(id INTEGER PRIMARY KEY,hash TEXT NOT NULL UNIQUE,hint TEXT NOT NULL,service TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'ready',verification_id TEXT NOT NULL DEFAULT '',phone TEXT NOT NULL DEFAULT '',code TEXT NOT NULL DEFAULT '',message TEXT NOT NULL DEFAULT '',ends_at TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL DEFAULT '');
 CREATE UNIQUE INDEX verification_unique ON keys(verification_id) WHERE verification_id<>'';
 INSERT INTO keys(hash,hint,service,verification_id) VALUES('old-hash','old-hint','openai','v-legacy')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		db, err = openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var full, kind, duration, reservation string
		err = db.QueryRow("SELECT full_key,reservation_type,duration,reservation_id FROM keys WHERE hash='old-hash'").Scan(&full, &kind, &duration, &reservation)
		if err != nil || full != "" || kind != TypeVerification || duration != "" || reservation != "v-legacy" {
			t.Fatalf("legacy migration failed: %v %q %q %q %q", err, full, kind, duration, reservation)
		}
		var newIndex, oldIndex int
		db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='reservation_unique'").Scan(&newIndex)
		db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='verification_unique'").Scan(&oldIndex)
		db.Close()
		if newIndex != 1 || oldIndex != 0 {
			t.Fatalf("index not migrated: new=%d old=%d", newIndex, oldIndex)
		}
	}
}
