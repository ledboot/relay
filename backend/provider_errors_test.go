package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPurchaseRejection(t *testing.T) {
	for _, body := range []string{`{"errorCode":"PriceLimit","errorDescription":"Price exceeds maximum"}`, `<html>bad request</html>`} {
		t.Run(body, func(t *testing.T) {
			var purchaseKey string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				purchaseKey = r.Header.Get("Idempotency-Key")
				w.WriteHeader(400)
				w.Write([]byte(body))
			}))
			defer server.Close()
			p := &TV{Base: server.URL, Client: server.Client(), token: "test", expiry: time.Now().Add(time.Hour), MaxPrice: 5}
			_, err := p.Create(context.Background(), "openai", "stable")
			upstream, ok := err.(*UpstreamError)
			if !ok || upstream.Status != 400 {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.HasPrefix(body, "{") && (upstream.Code != "PriceLimit" || !strings.Contains(err.Error(), "Price exceeds maximum")) {
				t.Fatalf("missing details: %v", err)
			}
			a, _, key := fixture(t)
			a.provider = p
			for i := 0; i < 2; i++ {
				result := call(a, "/api/redeem", key)
				if result.Code != 502 || !strings.Contains(result.Body.String(), "上游拒绝") {
					t.Fatal(result.Body.String())
				}
				if purchaseKey != digest(key) {
					t.Fatal("purchase retry key changed")
				}
			}
			var state, id string
			if err := a.db.QueryRow("SELECT state,verification_id FROM keys WHERE hash=?", digest(key)).Scan(&state, &id); err != nil {
				t.Fatal(err)
			}
			if state != "allocating" || id != "" {
				t.Fatalf("unexpected order state: %s %s", state, id)
			}
		})
	}
}
