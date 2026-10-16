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

type Verification struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	State  string `json:"state"`
	EndsAt string `json:"endsAt"`
}
type SMS struct {
	Code          string `json:"parsedCode"`
	Content       string `json:"smsContent"`
	ReservationID string `json:"reservationId"`
}
type Service struct {
	Name        string `json:"serviceName"`
	Description string `json:"description"`
	Capability  string `json:"capability"`
}
type Provider interface {
	Services(context.Context) ([]Service, error)
	Create(context.Context, string, string) (Verification, error)
	Get(context.Context, string) (Verification, []SMS, error)
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
	slog.InfoContext(ctx, "request", "method", method,"base", t.Base, "path", path, "key", key, "body", string(raw))
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
func (t *TV) Services(ctx context.Context) ([]Service, error) {
	var s []Service
	_, e := t.request(ctx, "GET", "/api/pub/v2/services?numberType=mobile&reservationType=verification", "", nil, &s)
	for i := range s {
		if strings.EqualFold(strings.TrimSpace(s[i].Capability), "sms") {
			s[i].Capability = "sms"
		}
	}
	return s, e
}
func (t *TV) Create(ctx context.Context, service, key string) (Verification, error) {
	loc, e := t.request(ctx, "POST", "/api/pub/v2/verifications", key, map[string]any{"serviceName": service, "capability": "sms", "maxPrice": t.MaxPrice}, nil)
	if e != nil {
		slog.ErrorContext(ctx, "create verification error", "error", e.Error())
		return Verification{}, e
	}
	u, e := url.Parse(loc)
	if e != nil || loc == "" {
		return Verification{}, fmt.Errorf("missing verification location")
	}
	// Only extract the ID; never forward credentials to a supplied Location host.
	prefix := "/api/pub/v2/verifications/"
	if !strings.HasPrefix(u.Path, prefix) {
		return Verification{}, fmt.Errorf("unexpected location")
	}
	id := strings.TrimPrefix(u.Path, prefix)
	if id == "" || strings.Contains(id, "/") {
		return Verification{}, fmt.Errorf("invalid verification ID")
	}
	return Verification{ID: id}, nil
}
func (t *TV) Get(ctx context.Context, id string) (Verification, []SMS, error) {
	var v Verification
	var messages struct {
		Data []SMS `json:"data"`
	}
	_, e := t.request(ctx, "GET", "/api/pub/v2/verifications/"+url.PathEscape(id), "", nil, &v)
	if e != nil {
		return v, nil, e
	}
	_, e = t.request(ctx, "GET", "/api/pub/v2/sms?reservationType=verification&reservationId="+url.QueryEscape(id), "", nil, &messages)
	return v, messages.Data, e
}
