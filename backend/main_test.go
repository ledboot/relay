package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRentalCreated is the moment the fake rental was bought; messages before it must not
// reach the customer.
const fakeRentalCreated = "2026-10-16T09:00:00Z"

type fakeProvider struct {
	mu        sync.Mutex
	purchases map[string]string
	sms       []SMS
	fail      bool
	// rentalSMS is what Get reports for rentals; rentals keep every message.
	rentalSMS []SMS
	// rentalState is what Get reports for rentals. Empty means still active.
	rentalState string
	// rentalPrice is quoted by PriceRental; tests raise it to exercise the cap.
	rentalPrice float64
	// createErr makes Create report a purchase that upstream accepted but cannot hand back
	// yet, e.g. a sale that is still processing.
	createErr error
}

func (f *fakeProvider) Services(context.Context, string) ([]Service, error) {
	return []Service{{Name: "openai", Capability: "sms"}}, nil
}
func (f *fakeProvider) PriceRental(context.Context, string, string) (float64, error) {
	return f.rentalPrice, nil
}
func (f *fakeProvider) Create(ctx context.Context, req PurchaseRequest, k string) (Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Replaying an idempotency key returns the original purchase and buys nothing,
	// which is what decides whether a retry really gets a new number.
	if id, ok := f.purchases[k]; ok {
		return Reservation{ID: id}, nil
	}
	prefix := "v"
	if isRental(req.Type) {
		prefix = "r"
	}
	id := prefix + strconv.Itoa(len(f.purchases)+1)
	f.purchases[k] = id
	if f.createErr != nil {
		return Reservation{}, f.createErr
	}
	return Reservation{ID: id}, nil
}
func (f *fakeProvider) Get(_ context.Context, reservationType, id string) (Reservation, []SMS, error) {
	if isRental(reservationType) {
		state := f.rentalState
		if state == "" {
			state = "nonrenewableActive"
		}
		return Reservation{ID: id, Number: "15555550200", State: state, EndsAt: "2099-01-01T00:00:00Z", CreatedAt: fakeRentalCreated}, f.rentalSMS, nil
	}
	return Reservation{ID: "v1", Number: "15555550100", State: "verificationPending"}, f.sms, nil
}
func fixture(t *testing.T) (*App, *fakeProvider, string) {
	t.Helper()
	db, e := openDB(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	f := &fakeProvider{purchases: map[string]string{}}
	a := &App{db: db, provider: f, admin: "test-admin", secret: "whsec_test", maxRetries: 1, maxRentalPrice: 30, limitPerMin: 120, keyLimitPerMin: 120, limiter: newLimiter(maxIdentities)}
	key := randomKey("RELAY", "openai")
	_, e = db.Exec("INSERT INTO keys(hash,hint,service) VALUES(?,?,?)", digest(key), "hint", "openai")
	if e != nil {
		t.Fatal(e)
	}
	return a, f, key
}
func call(a *App, path, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(`{"key":"`+key+`"}`))
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}
func TestConcurrentRedeem(t *testing.T) {
	a, f, key := fixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := call(a, "/api/redeem", key)
			if w.Code != 200 {
				t.Errorf("%d %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if len(f.purchases) != 1 {
		t.Fatal("duplicate purchase")
	}
	f.sms = []SMS{{Code: "123456", Content: "Your code: 123456"}}
	w := call(a, "/api/status", key)
	var k Key
	json.Unmarshal(w.Body.Bytes(), &k)
	if k.State != "used" || k.Code != "123456" {
		t.Fatal(w.Body.String())
	}
	assertAdminSMS(t, a, "123456", "Your code: 123456")
	call(a, "/api/redeem", key)
	if len(f.purchases) != 1 {
		t.Fatal("used key repurchased")
	}
}
func TestWebhookSignatureAndDedup(t *testing.T) {
	a, _, key := fixture(t)
	call(a, "/api/redeem", key)
	// A delayed SMS callback must still be retained after timeout.
	a.db.Exec("UPDATE keys SET state='timed_out'")
	body := `{"id":"event1","idempotencyKey":"same","event":"v2.sms.received","data":{"reservationId":"v1","parsedCode":"654321","smsContent":"code 654321"}}`
	send := func(signature string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/webhooks/textverified", strings.NewReader(body))
		r.Header.Set("X-Webhook-Signature", signature)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	if send("bad").Code != 401 {
		t.Fatal("accepted invalid signature")
	}
	h := hmac.New(sha512.New, []byte(a.secret))
	h.Write([]byte(body))
	signature := "HMAC-SHA512=" + base64.StdEncoding.EncodeToString(h.Sum(nil))
	for i := 0; i < 2; i++ {
		if w := send(signature); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count)
	if count != 1 {
		t.Fatal("event not deduplicated")
	}
	w := call(a, "/api/status", key)
	if !strings.Contains(w.Body.String(), `"state":"used"`) {
		t.Fatal(w.Body.String())
	}
	assertAdminSMS(t, a, "654321", "code 654321")
}
func assertAdminSMS(t *testing.T, a *App, code, message string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/admin/keys?page=1", nil)
	req.Header.Set("Authorization", "Bearer test-admin")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, req)
	var result struct{ Items []adminKey }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(result.Items) != 1 || result.Items[0].Code != code || result.Items[0].Message != message {
		t.Fatalf("admin SMS record not retained: %s", w.Body.String())
	}
}
func TestUnauthorizedAndDisabled(t *testing.T) {
	a, _, key := fixture(t)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/keys", nil))
	if w.Code != 401 {
		t.Fatal("admin unprotected")
	}
	a.db.Exec("UPDATE keys SET state='disabled'")
	if call(a, "/api/redeem", key).Code != 403 {
		t.Fatal("disabled redeemed")
	}
}
func TestProviderContract(t *testing.T) {
	var auth, purchases int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pub/v2/auth":
			auth++
			if r.Header.Get("X-API-KEY") != "key" {
				t.Error("missing key")
			}
			w.Write([]byte(`{"token":"bearer","expiresAt":"2099-01-01T00:00:00Z"}`))
		case "/api/pub/v2/verifications":
			purchases++
			if r.Header.Get("Idempotency-Key") != "stable" || r.Header.Get("Authorization") != "Bearer bearer" {
				t.Error("missing headers")
			}
			w.Header().Set("Location", "/api/pub/v2/verifications/v1")
			w.WriteHeader(201)
		case "/api/pub/v2/verifications/v1":
			w.Write([]byte(`{"id":"v1","number":"123","state":"verificationPending"}`))
		case "/api/pub/v2/sms":
			if r.URL.Query().Get("reservationId") != "v1" {
				t.Error("wrong sms filter")
			}
			w.Write([]byte(`{"data":[{"parsedCode":"123456"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	p := &TV{Base: s.URL, Key: "key", Client: s.Client(), MaxPrice: 5}
	v, e := p.Create(context.Background(), PurchaseRequest{Service: "openai", Type: TypeVerification}, "stable")
	if e != nil {
		t.Fatal(e)
	}
	_, sms, e := p.Get(context.Background(), TypeVerification, v.ID)
	if e != nil || len(sms) != 1 || auth != 1 || purchases != 1 {
		t.Fatalf("contract failure %v", e)
	}
}
