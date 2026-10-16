package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestLoggingLevel(t *testing.T) {
	var output bytes.Buffer
	logger, err := newLogger(&output, "json", "WARN")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Warn("visible")
	if strings.Contains(output.String(), "hidden") || !strings.Contains(output.String(), "visible") {
		t.Fatal(output.String())
	}
	for _, config := range [][2]string{{"bad", "INFO"}, {"json", "bad"}} {
		if _, err := newLogger(&output, config[0], config[1]); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}

func TestRequestLogsAndRedaction(t *testing.T) {
	var output bytes.Buffer
	logger, _ := newLogger(&output, "json", "INFO")
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)
	a, _, _ := fixture(t)
	handler := a.routes()
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/health?token=secret-value", 200},
		{"/api/admin/keys", 401},
		{"/secret-value?token=secret-value", 404},
	} {
		output.Reset()
		request := httptest.NewRequest("GET", tc.path, strings.NewReader("secret-value"))
		request.Header.Set("Authorization", "secret-value")
		request.Header.Set("X-Request-ID", "secret-value")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("status %d", response.Code)
		}
		assertRequestLog(t, &output, response, tc.status)
	}
	a.limiter.buckets["ip:direct"] = bucket{count: a.limitPerMin, at: time.Now()}
	output.Reset()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/status", nil))
	assertRequestLog(t, &output, response, 429)

	output.Reset()
	router := gin.New()
	router.Use(requestLogging())
	router.GET("/panic", func(c *gin.Context) { panic("secret-value") })
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/panic", nil))
	assertRequestLog(t, &output, response, http.StatusInternalServerError)
	if !strings.Contains(output.String(), `"panic":true`) {
		t.Fatal(output.String())
	}
}

func assertRequestLog(t *testing.T, output *bytes.Buffer, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if strings.Contains(output.String(), "secret-value") {
		t.Fatal("sensitive information leaked")
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if response.Code != status || record["status"] != float64(status) {
		t.Fatalf("unexpected status: %s", output.String())
	}
	id := response.Header().Get("X-Request-ID")
	if len(id) != 32 || record["request_id"] != id {
		t.Fatal("request ID missing or mismatched")
	}
}
