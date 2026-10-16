package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Reservation types mirror the upstream reservationType values, which double as the
// `reservationType` query parameter of the service list and the path segment of the
// rental endpoints. Only verification and non-renewable rentals are sold today;
// renewable is kept so the type can be enabled without reshaping stored rows.
const (
	TypeVerification = "verification"
	TypeNonrenewable = "nonrenewable"
	TypeRenewable    = "renewable"
)

// isRental reports whether a reservation type holds a number for a period instead of a
// single verification. Rentals follow a different state machine and never store the
// messages they receive.
func isRental(reservationType string) bool { return reservationType != TypeVerification }

// purchasableType reports whether a reservation type can be sold with the flows that are
// implemented today. Renewable rentals need billing-cycle handling and are rejected.
func purchasableType(reservationType string) bool {
	return reservationType == TypeVerification || reservationType == TypeNonrenewable
}

// rentalDurations are the periods accepted by the upstream rental API.
var rentalDurations = map[string]bool{
	"oneDay": true, "threeDay": true, "sevenDay": true, "fourteenDay": true,
	"thirtyDay": true, "ninetyDay": true, "oneYear": true,
}

// Reservation is the upstream order this service tracks. Rentals and verifications share
// the shape: an ID, the number, the upstream state and the end of the usable period.
type Reservation struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	State  string `json:"state"`
	EndsAt string `json:"endsAt"`
	// CreatedAt is when the reservation was created upstream. For a rental it is the
	// boundary that keeps messages from the number's earlier life out of the response.
	CreatedAt string `json:"createdAt"`
}

// PurchaseRequest describes one upstream purchase.
type PurchaseRequest struct {
	Service  string
	Type     string
	Duration string
}

type SMS struct {
	ID            string `json:"id"`
	Code          string `json:"parsedCode"`
	Content       string `json:"smsContent"`
	ReservationID string `json:"reservationId"`
	CreatedAt     string `json:"createdAt"`
}
type Service struct {
	Name        string `json:"serviceName"`
	Description string `json:"description"`
	Capability  string `json:"capability"`
}
type Provider interface {
	// Services lists the services that can be bought for a reservation type.
	Services(ctx context.Context, reservationType string) ([]Service, error)
	// PriceRental quotes a rental so the purchase can be capped before any spend.
	// Upstream accepts no maxPrice on a rental purchase, so this is the only guard.
	PriceRental(ctx context.Context, service, duration string) (float64, error)
	// Create buys a reservation. The idempotency key must be stable for one purchase.
	Create(ctx context.Context, req PurchaseRequest, idempotencyKey string) (Reservation, error)
	// Get returns the current state, and for verifications the messages received so far.
	Get(ctx context.Context, reservationType, id string) (Reservation, []SMS, error)
}

// UpstreamError retains the documented error fields without logging raw responses.
type UpstreamError struct {
	Status      int
	Code        string `json:"errorCode"`
	Description string `json:"errorDescription"`
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream status %d code=%q description=%q", e.Status, e.Code, e.Description)
}

type TV struct {
	Base, Key, Username string
	MaxPrice            float64
	Client              *http.Client
	mu                  sync.Mutex
	token               string
	expiry              time.Time
}

func (t *TV) bearer(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Add(time.Minute).Before(t.expiry) {
		return t.token, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", t.Base+"/api/pub/v2/auth", nil)
	req.Header.Set("X-API-KEY", t.Key)
	req.Header.Set("X-API-USERNAME", t.Username)
	res, err := t.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("authentication status %d", res.StatusCode)
	}
	var b struct {
		Token     string
		ExpiresAt time.Time
	}
	err = json.NewDecoder(res.Body).Decode(&b)
	if err != nil {
		return "", err
	}
	if b.Token == "" {
		return "", fmt.Errorf("empty bearer")
	}
	t.token = b.Token
	t.expiry = b.ExpiresAt
	return t.token, nil
}
func (t *TV) request(ctx context.Context, method, path, key string, body any, out any) (string, error) {
	token, err := t.bearer(ctx)
	if err != nil {
		return "", err
	}
	var raw []byte
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return "", err
		}
	}
	slog.InfoContext(ctx, "request", "method", method, "base", t.Base, "path", path, "key", key, "body", string(raw))
	req, err := http.NewRequestWithContext(ctx, method, t.Base+path, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := t.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if res.StatusCode == 401 {
			t.mu.Lock()
			t.expiry = time.Time{}
			t.mu.Unlock()
		}
		upstream := &UpstreamError{Status: res.StatusCode}
		// Bound response decoding; HTML or malformed responses retain the status.
		_ = json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(upstream)
		return "", upstream
	}
	if out != nil {
		err = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(out)
	}
	return res.Header.Get("Location"), err
}

// idFromLocation extracts the trailing resource ID from a returned Location header. Only
// the ID is read; credentials are never forwarded to a host supplied by the response.
func idFromLocation(loc, prefix string) (string, error) {
	if loc == "" {
		return "", fmt.Errorf("missing location")
	}
	u, err := url.Parse(loc)
	if err != nil || !strings.HasPrefix(u.Path, prefix) {
		return "", fmt.Errorf("unexpected location")
	}
	id := strings.TrimPrefix(u.Path, prefix)
	if id == "" || strings.Contains(id, "/") {
		return "", fmt.Errorf("invalid id")
	}
	return id, nil
}

func (t *TV) Services(ctx context.Context, reservationType string) ([]Service, error) {
	var s []Service
	q := url.Values{}
	q.Set("numberType", "mobile")
	q.Set("reservationType", reservationType)
	_, e := t.request(ctx, "GET", "/api/pub/v2/services?"+q.Encode(), "", nil, &s)
	for i := range s {
		if strings.EqualFold(strings.TrimSpace(s[i].Capability), "sms") {
			s[i].Capability = "sms"
		}
	}
	return s, e
}

func (t *TV) PriceRental(ctx context.Context, service, duration string) (float64, error) {
	var p struct {
		Price float64 `json:"price"`
	}
	_, e := t.request(ctx, "POST", "/api/pub/v2/pricing/rentals", "", map[string]any{
		"serviceName": service,
		"duration":    duration,
		"isRenewable": false,
		"numberType":  "mobile",
		"capability":  "sms",
		"areaCode":    false,
	}, &p)
	return p.Price, e
}

func (t *TV) Create(ctx context.Context, req PurchaseRequest, key string) (Reservation, error) {
	if isRental(req.Type) {
		return t.createRental(ctx, req, key)
	}
	return t.createVerification(ctx, req.Service, key)
}

func (t *TV) createVerification(ctx context.Context, service, key string) (Reservation, error) {
	loc, e := t.request(ctx, "POST", "/api/pub/v2/verifications", key, map[string]any{"serviceName": service, "capability": "sms", "maxPrice": t.MaxPrice}, nil)
	if e != nil {
		slog.ErrorContext(ctx, "create verification error", "error", e.Error())
		return Reservation{}, e
	}
	id, e := idFromLocation(loc, "/api/pub/v2/verifications/")
	if e != nil {
		return Reservation{}, fmt.Errorf("missing verification location")
	}
	return Reservation{ID: id}, nil
}

// createRental buys a rental and follows the returned sale to the created rental. The sale
// can still be processing, in which case the caller keeps the key in `allocating` and the
// next attempt replays the same idempotency key to read the same sale again.
func (t *TV) createRental(ctx context.Context, req PurchaseRequest, key string) (Reservation, error) {
	loc, e := t.request(ctx, "POST", "/api/pub/v2/reservations/rental", key, map[string]any{
		"serviceName":                req.Service,
		"duration":                   req.Duration,
		"isRenewable":                req.Type == TypeRenewable,
		"numberType":                 "mobile",
		"capability":                 "sms",
		"allowBackOrderReservations": false,
	}, nil)
	if e != nil {
		slog.ErrorContext(ctx, "create rental error", "error", e.Error())
		return Reservation{}, e
	}
	saleID, e := idFromLocation(loc, "/api/pub/v2/sales/")
	if e != nil {
		return Reservation{}, fmt.Errorf("missing sale location")
	}
	var sale struct {
		State        string `json:"state"`
		Reservations []struct {
			ID              string `json:"id"`
			ReservationType string `json:"reservationType"`
		} `json:"reservations"`
	}
	if _, e = t.request(ctx, "GET", "/api/pub/v2/sales/"+url.PathEscape(saleID), "", nil, &sale); e != nil {
		return Reservation{}, e
	}
	for _, r := range sale.Reservations {
		if r.ID != "" && isRental(r.ReservationType) {
			return Reservation{ID: r.ID}, nil
		}
	}
	return Reservation{}, fmt.Errorf("sale %s has no rental yet", saleID)
}

func (t *TV) Get(ctx context.Context, reservationType, id string) (Reservation, []SMS, error) {
	var v Reservation
	var messages struct {
		Data []SMS `json:"data"`
	}
	if isRental(reservationType) {
		_, e := t.request(ctx, "GET", "/api/pub/v2/reservations/rental/"+url.PathEscape(reservationType)+"/"+url.PathEscape(id), "", nil, &v)
		if e != nil {
			return v, nil, e
		}
		// A rental keeps every message it receives, so the caller stores the list instead
		// of treating one message as the result of the order.
		_, e = t.request(ctx, "GET", "/api/pub/v2/sms?reservationType="+url.QueryEscape(reservationType)+"&reservationId="+url.QueryEscape(id), "", nil, &messages)
		return v, messages.Data, e
	}
	_, e := t.request(ctx, "GET", "/api/pub/v2/verifications/"+url.PathEscape(id), "", nil, &v)
	if e != nil {
		return v, nil, e
	}
	_, e = t.request(ctx, "GET", "/api/pub/v2/sms?reservationType="+url.QueryEscape(reservationType)+"&reservationId="+url.QueryEscape(id), "", nil, &messages)
	return v, messages.Data, e
}
