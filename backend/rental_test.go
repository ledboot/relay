package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rentalFixture adds an unspent non-renewable rental key next to the verification key
// created by fixture.
func rentalFixture(t *testing.T) (*App, *fakeProvider, string) {
	t.Helper()
	a, f, _ := fixture(t)
	key := randomKey("RELAY", "openai")
	if _, e := a.db.Exec("INSERT INTO keys(hash,hint,service,reservation_type,duration) VALUES(?,?,?,?,?)",
		digest(key), "hint", "openai", TypeNonrenewable, "thirtyDay"); e != nil {
		t.Fatal(e)
	}
	return a, f, key
}

func rentalKey(t *testing.T, a *App, key string) Key {
	t.Helper()
	k, e := scan(a.db.QueryRow("SELECT "+columns+" FROM keys WHERE hash=?", digest(key)))
	if e != nil {
		t.Fatal(e)
	}
	return k
}

// tableExists reports whether a table is present in the test database.
func tableExists(t *testing.T, a *App, name string) bool {
	t.Helper()
	var n int
	if e := a.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n > 0
}

func TestRentalMessagesAreLiveNewestFirstAndFiltered(t *testing.T) {
	a, f, key := rentalFixture(t)
	// The number's earlier life must never show up, and the upstream order is arbitrary.
	f.rentalSMS = []SMS{
		{ID: "old", Code: "000000", Content: "before this rental", CreatedAt: "2026-10-16T08:00:00Z"},
		{ID: "m1", Code: "111111", Content: "first", CreatedAt: "2026-10-16T10:00:00Z"},
		{ID: "m2", Code: "222222", Content: "second", CreatedAt: "2026-10-16T10:05:00+00:00"},
	}
	w := call(a, "/api/redeem", key)
	if w.Code != 200 {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	var k Key
	if e := json.Unmarshal(w.Body.Bytes(), &k); e != nil {
		t.Fatal(e)
	}
	if k.State != "rental_active" || k.Phone != "15555550200" || k.ReservationType != TypeNonrenewable || k.Duration != "thirtyDay" {
		t.Fatalf("unexpected rental order: %+v", k)
	}
	if len(k.Messages) != 2 || k.Messages[0].Code != "222222" || k.Messages[1].Code != "111111" {
		t.Fatalf("messages not filtered and sorted newest first: %+v", k.Messages)
	}
	if k.Messages[0].Content != "second" || k.Messages[0].At == "" {
		t.Fatalf("message content or time missing: %+v", k.Messages[0])
	}
	// The flat code/message columns stay reserved for verifications.
	stored := rentalKey(t, a, key)
	if stored.ReservationID != "r1" || stored.Code != "" || stored.Message != "" {
		t.Fatalf("unexpected stored rental: %+v", stored)
	}
	// A verification code is never written to disk.
	if tableExists(t, a, "messages") {
		t.Fatal("rental messages must not be stored")
	}
	// The list is rebuilt from upstream on every poll, so a new message lands on top.
	f.rentalSMS = append([]SMS{{ID: "m3", Code: "333333", Content: "third", CreatedAt: "2026-10-16T10:09:00Z"}}, f.rentalSMS...)
	w = call(a, "/api/status", key)
	json.Unmarshal(w.Body.Bytes(), &k)
	if len(k.Messages) != 3 || k.Messages[0].Code != "333333" {
		t.Fatalf("new message not inserted first: %+v", k.Messages)
	}
	// A finished rental still returns its messages, because they are read live.
	f.rentalState = "nonrenewableExpired"
	w = call(a, "/api/status", key)
	json.Unmarshal(w.Body.Bytes(), &k)
	if k.State != "rental_expired" || len(k.Messages) != 3 {
		t.Fatalf("expired rental lost its messages: %+v", k)
	}
	if len(f.purchases) != 1 {
		t.Fatal("rental purchased more than once")
	}
}

// A long rental keeps receiving messages; the response stays small.
func TestRentalMessagesAreCapped(t *testing.T) {
	a, f, key := rentalFixture(t)
	for i := 0; i < 30; i++ {
		f.rentalSMS = append(f.rentalSMS, SMS{
			ID: fmt.Sprint(i), Code: fmt.Sprintf("%06d", i), Content: "x",
			CreatedAt: fmt.Sprintf("2026-10-16T10:%02d:00Z", i),
		})
	}
	var k Key
	json.Unmarshal(call(a, "/api/redeem", key).Body.Bytes(), &k)
	if len(k.Messages) != maxRentalMessages {
		t.Fatalf("expected %d messages, got %d", maxRentalMessages, len(k.Messages))
	}
	if k.Messages[0].Code != "000029" {
		t.Fatalf("cap kept the wrong end: %+v", k.Messages[0])
	}
}

func TestRentalRefreshTracksExpiry(t *testing.T) {
	a, f, key := rentalFixture(t)
	if w := call(a, "/api/redeem", key); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for upstream, want := range map[string]string{
		"nonrenewableActive":   "rental_active",
		"nonrenewableExpired":  "rental_expired",
		"nonrenewableRefunded": "rental_refunded",
	} {
		f.rentalState = upstream
		a.db.Exec("UPDATE keys SET state='rental_active' WHERE hash=?", digest(key))
		w := call(a, "/api/status", key)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"`+want+`"`) {
			t.Fatalf("%s -> %d %s", upstream, w.Code, w.Body.String())
		}
	}
	// Expiry is a normal end of life, not an error, so it stays queryable and selectable.
	a.db.Exec("UPDATE keys SET state='rental_expired' WHERE hash=?", digest(key))
	req := httptest.NewRequest("GET", "/api/admin/keys?page=1&state=rental_expired", nil)
	req.Header.Set("Authorization", "Bearer test-admin")
	result := httptest.NewRecorder()
	a.routes().ServeHTTP(result, req)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"total":1`) {
		t.Fatalf("admin filter: %d %s", result.Code, result.Body.String())
	}
}

// A rental's messages are read live and never stored, so a callback must neither consume the
// key nor write anything.
func TestRentalWebhookDoesNotStoreOrConsume(t *testing.T) {
	a, f, key := rentalFixture(t)
	call(a, "/api/redeem", key)
	send := func(eventID string) *httptest.ResponseRecorder {
		body := `{"id":"` + eventID + `","event":"v2.sms.received","data":{"reservationId":"r1","parsedCode":"654321","smsContent":"code 654321","createdAt":"2026-10-16T11:00:00Z"}}`
		sign := hmac.New(sha512.New, []byte(a.secret))
		sign.Write([]byte(body))
		r := httptest.NewRequest("POST", "/api/webhooks/textverified", strings.NewReader(body))
		r.Header.Set("X-Webhook-Signature", "HMAC-SHA512="+base64.StdEncoding.EncodeToString(sign.Sum(nil)))
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	if w := send("event1"); w.Code != 200 {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}
	k := rentalKey(t, a, key)
	if k.State != "rental_active" || k.Code != "" || k.Message != "" {
		t.Fatalf("rental consumed by webhook: %+v", k)
	}
	if tableExists(t, a, "messages") {
		t.Fatal("webhook wrote a message to disk")
	}
	// The callback only reaches the customer once upstream reports the same message.
	var order Key
	json.Unmarshal(call(a, "/api/status", key).Body.Bytes(), &order)
	if len(order.Messages) != 0 {
		t.Fatalf("callback stored a message: %+v", order.Messages)
	}
	f.rentalSMS = []SMS{{Code: "654321", Content: "code 654321", CreatedAt: "2026-10-16T11:00:00Z"}}
	json.Unmarshal(call(a, "/api/status", key).Body.Bytes(), &order)
	if len(order.Messages) != 1 || order.Messages[0].Code != "654321" {
		t.Fatalf("message missing from the live list: %+v", order.Messages)
	}
}

func TestRentalPriceCapBlocksSpend(t *testing.T) {
	a, f, key := rentalFixture(t)
	f.rentalPrice = 100
	w := call(a, "/api/redeem", key)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "超过上限") {
		t.Fatalf("over-cap rental bought: %d %s", w.Code, w.Body.String())
	}
	if len(f.purchases) != 0 {
		t.Fatal("spend happened despite the price cap")
	}
	if k := rentalKey(t, a, key); k.State != "ready" {
		t.Fatalf("key left the ready state: %+v", k)
	}
	// Raising the cap lets the same untouched key buy.
	a.maxRentalPrice = 150
	if w := call(a, "/api/redeem", key); w.Code != 200 {
		t.Fatalf("redeem after raising the cap: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminRejectsUnsupportedRentalTypes(t *testing.T) {
	a, _, _ := fixture(t)
	generate := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/admin/keys", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-admin")
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"rental without duration", `{"service":"openai","type":"nonrenewable","count":1}`, 400},
		{"rental with unknown duration", `{"service":"openai","type":"nonrenewable","duration":"twoDay","count":1}`, 400},
		{"renewable rental", `{"service":"openai","type":"renewable","duration":"thirtyDay","count":1}`, 400},
		{"unknown type", `{"service":"openai","type":"lifetime","count":1}`, 400},
		{"verification ignores duration", `{"service":"openai","type":"verification","duration":"thirtyDay","count":1}`, 201},
		{"nonrenewable rental", `{"service":"openai","type":"nonrenewable","duration":"thirtyDay","count":1}`, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := generate(tc.body)
			if w.Code != tc.code {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.code != 201 {
				return
			}
			var created struct {
				Keys []string `json:"keys"`
			}
			json.Unmarshal(w.Body.Bytes(), &created)
			var section, duration string
			if e := a.db.QueryRow("SELECT reservation_type,duration FROM keys WHERE hash=?", digest(created.Keys[0])).Scan(&section, &duration); e != nil {
				t.Fatal(e)
			}
			switch tc.name {
			case "verification ignores duration":
				if section != TypeVerification || duration != "" {
					t.Fatalf("verification stored a rental period: %q %q", section, duration)
				}
			default:
				if section != TypeNonrenewable || duration != "thirtyDay" {
					t.Fatalf("rental not stored: %q %q", section, duration)
				}
			}
		})
	}
}

// TestRentalProviderContract pins the upstream calls a rental purchase makes: price,
// create, follow the sale, then read the rental and its messages.
func TestRentalProviderContract(t *testing.T) {
	var smsCalls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pub/v2/auth":
			w.Write([]byte(`{"token":"bearer","expiresAt":"2099-01-01T00:00:00Z"}`))
		case "/api/pub/v2/services":
			if r.URL.Query().Get("reservationType") != TypeNonrenewable {
				t.Errorf("wrong service type: %s", r.URL)
			}
			w.Write([]byte(`[{"serviceName":"openai","capability":"sms"}]`))
		case "/api/pub/v2/pricing/rentals":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["duration"] != "thirtyDay" || body["isRenewable"] != false || body["serviceName"] != "openai" {
				t.Errorf("wrong pricing body: %v", body)
			}
			w.Write([]byte(`{"serviceName":"openai","price":12.5}`))
		case "/api/pub/v2/reservations/rental":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Idempotency-Key") != "stable" || body["duration"] != "thirtyDay" || body["isRenewable"] != false || body["allowBackOrderReservations"] != false {
				t.Errorf("wrong rental request: %v %v", body, r.Header.Get("Idempotency-Key"))
			}
			w.Header().Set("Location", "/api/pub/v2/sales/s1")
			w.WriteHeader(201)
		case "/api/pub/v2/sales/s1":
			w.Write([]byte(`{"state":"succeeded","reservations":[{"id":"r1","reservationType":"nonrenewable"}]}`))
		case "/api/pub/v2/reservations/rental/nonrenewable/r1":
			w.Write([]byte(`{"id":"r1","number":"15551234567","state":"nonrenewableActive","endsAt":"2099-01-01T00:00:00Z"}`))
		case "/api/pub/v2/sms":
			smsCalls++
			if r.URL.Query().Get("reservationType") != TypeNonrenewable || r.URL.Query().Get("reservationId") != "r1" {
				t.Errorf("wrong sms filter: %s", r.URL)
			}
			w.Write([]byte(`{"data":[{"id":"m1","parsedCode":"999999","smsContent":"hi","createdAt":"2026-10-16T10:00:00Z"}]}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	p := &TV{Base: s.URL, Client: s.Client(), MaxPrice: 5, token: "test", expiry: time.Now().Add(time.Hour)}
	if _, e := p.Services(context.Background(), TypeNonrenewable); e != nil {
		t.Fatal(e)
	}
	price, e := p.PriceRental(context.Background(), "openai", "thirtyDay")
	if e != nil || price != 12.5 {
		t.Fatalf("price: %v %v", price, e)
	}
	r, e := p.Create(context.Background(), PurchaseRequest{Service: "openai", Type: TypeNonrenewable, Duration: "thirtyDay"}, "stable")
	if e != nil || r.ID != "r1" {
		t.Fatalf("create rental: %+v %v", r, e)
	}
	got, sms, e := p.Get(context.Background(), TypeNonrenewable, r.ID)
	if e != nil || got.Number != "15551234567" || got.State != "nonrenewableActive" || len(sms) != 1 || sms[0].Code != "999999" {
		t.Fatalf("get rental: %+v %v", got, e)
	}
	if smsCalls != 1 {
		t.Fatalf("rental lookup sms calls: %d", smsCalls)
	}
}

// A sale that is still processing has no rental yet. The key must stay allocating so the
// next attempt replays the same idempotency key instead of buying a second rental.
func TestRentalPurchaseWaitsForSale(t *testing.T) {
	a, f, key := rentalFixture(t)
	f.createErr = errors.New("sale still processing")
	if w := call(a, "/api/redeem", key); w.Code != 502 {
		t.Fatalf("pending sale should be retryable: %d %s", w.Code, w.Body.String())
	}
	if k := rentalKey(t, a, key); k.State != "allocating" || k.ReservationID != "" {
		t.Fatalf("unexpected state after a pending sale: %+v", k)
	}
	if len(f.purchases) != 1 {
		t.Fatalf("purchases: %d", len(f.purchases))
	}
	f.createErr = nil
	if w := call(a, "/api/redeem", key); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"rental_active"`) {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	// The replay must reuse the same idempotency key: one purchase, not two.
	if len(f.purchases) != 1 {
		t.Fatalf("resume bought a second rental: %d", len(f.purchases))
	}
}
