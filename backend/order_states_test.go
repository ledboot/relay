package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type stateProvider struct {
	*fakeProvider
	state string
	err   error
}

func (p *stateProvider) Get(_ context.Context, reservationType, id string) (Reservation, []SMS, error) {
	// stateProvider drives verification states; rentals are not part of these cases.
	if isRental(reservationType) {
		return Reservation{ID: id}, nil, p.err
	}
	return Reservation{ID: "v1", Number: "15555550100", State: p.state}, p.sms, p.err
}
func TestOrderEndStates(t *testing.T) {
	for upstream, want := range map[string]string{"verificationTimedOut": "timed_out", "verificationCanceled": "canceled", "verificationRefunded": "refunded", "verificationReported": "reported"} {
		for _, initial := range []string{"waiting", "expired"} {
			t.Run(upstream+initial, func(t *testing.T) {
				a, f, key := fixture(t)
				a.provider = &stateProvider{fakeProvider: f, state: upstream}
				_, err := a.db.Exec("UPDATE keys SET state=?,reservation_id='v1'", initial)
				if err != nil {
					t.Fatal(err)
				}
				w := call(a, "/api/status", key)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"`+want+`"`) || !strings.Contains(w.Body.String(), `"phone":"15555550100"`) {
					t.Fatal(w.Body.String())
				}
				req := httptest.NewRequest("GET", "/api/admin/keys?page=1&state="+want, nil)
				req.Header.Set("Authorization", "Bearer test-admin")
				result := httptest.NewRecorder()
				a.routes().ServeHTTP(result, req)
				if result.Code != 200 || !strings.Contains(result.Body.String(), `"total":1`) {
					t.Fatal(result.Body.String())
				}
			})
		}
	}
}
func TestSMSWinsOverTimeout(t *testing.T) {
	a, f, key := fixture(t)
	a.provider = &stateProvider{fakeProvider: f, state: "verificationTimedOut"}
	f.sms = []SMS{{Code: "123456", Content: "code 123456"}}
	a.db.Exec("UPDATE keys SET state='waiting',reservation_id='v1'")
	w := call(a, "/api/status", key)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"used"`) {
		t.Fatal(w.Body.String())
	}
	assertAdminSMS(t, a, "123456", "code 123456")
}
func TestQueryFailureDoesNotMeanTimeout(t *testing.T) {
	a, f, key := fixture(t)
	a.provider = &stateProvider{fakeProvider: f, err: errors.New("network timeout")}
	a.db.Exec("UPDATE keys SET state='waiting',reservation_id='v1'")
	if w := call(a, "/api/status", key); w.Code != 502 {
		t.Fatal(w.Code)
	}
	var state string
	a.db.QueryRow("SELECT state FROM keys").Scan(&state)
	if state != "waiting" {
		t.Fatal(state)
	}
}
