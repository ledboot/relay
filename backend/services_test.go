package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServiceCapabilityNormalization(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pub/v2/auth" {
			w.Write([]byte(`{"token":"test","expiresAt":"2099-01-01T00:00:00Z"}`))
			return
		}
		if r.URL.Path != "/api/pub/v2/services" || r.URL.Query().Get("numberType") != "mobile" || r.URL.Query().Get("reservationType") != "verification" {
			t.Errorf("unexpected service request: %s", r.URL)
		}
		w.Write([]byte(`[{"serviceName":"openai","capability":"Sms"},{"serviceName":"telegram","capability":"sms"},{"serviceName":"voice-only","capability":"Voice"}]`))
	}))
	defer s.Close()
	p := &TV{Base: s.URL, Client: s.Client()}
	services, err := p.Services(context.Background(), TypeVerification)
	if err != nil || len(services) != 3 {
		t.Fatalf("services: %v, %v", services, err)
	}
	if services[0].Capability != "sms" || services[1].Capability != "sms" || services[2].Capability != "Voice" {
		t.Fatalf("incorrect normalization: %+v", services)
	}
}

type capabilityProvider struct {
	*fakeProvider
	capability string
}

func (p capabilityProvider) Services(context.Context, string) ([]Service, error) {
	return []Service{{Name: "openai", Capability: p.capability}}, nil
}

func TestGenerateKeysWithServiceCapabilities(t *testing.T) {
	for _, tc := range []struct {
		capability string
		status     int
	}{{"Sms", 201}, {"sms", 201}, {"SMS", 201}, {"Voice", 400}, {"", 400}} {
		t.Run(tc.capability, func(t *testing.T) {
			a, f, _ := fixture(t)
			a.provider = capabilityProvider{f, tc.capability}
			r := httptest.NewRequest("POST", "/api/admin/keys", strings.NewReader(`{"service":"openai","count":1,"brandPrefix":"RELAY"}`))
			r.Header.Set("Authorization", "Bearer test-admin")
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
