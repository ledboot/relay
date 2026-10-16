package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type requestIDKey struct{}

func newLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var severity slog.Level
	if err := severity.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("LOG_LEVEL must be DEBUG, INFO, WARN or ERROR")
	}
	options := &slog.HandlerOptions{Level: severity}
	switch strings.ToLower(format) {
	case "text":
		return slog.New(slog.NewTextHandler(w, options)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, options)), nil
	default:
		return nil, fmt.Errorf("LOG_FORMAT must be text or json")
	}
}

func configureLogging() error {
	logger, err := newLogger(os.Stdout, env("LOG_FORMAT", "text"), env("LOG_LEVEL", "INFO"))
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	return nil
}

// Error messages may contain upstream URLs, credentials or response data.
func errorType(err error) string { return fmt.Sprintf("%T", err) }

func cRequestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

func requestLogging() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			slog.Error("request ID generation failed", "error_type", errorType(err))
			c.Abort()
			fail(c.Writer, http.StatusInternalServerError, "服务内部错误")
			return
		}
		id := hex.EncodeToString(random[:])
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey{}, id))
		c.Header("X-Request-ID", id)
		defer func() {
			panicked := recover() != nil
			if panicked {
				c.Abort()
				if !c.Writer.Written() {
					fail(c.Writer, 500, "服务内部错误")
				}
			}
			// Only log registered route templates, never user-controlled URLs or queries.
			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}
			level := slog.LevelInfo
			if c.Writer.Status() >= 500 || panicked {
				level = slog.LevelError
			} else if c.Writer.Status() >= 400 {
				level = slog.LevelWarn
			}
			slog.Log(c.Request.Context(), level, "HTTP request", "request_id", id,
				"method", c.Request.Method, "route", route, "status", c.Writer.Status(),
				"duration_ms", float64(time.Since(start).Microseconds())/1000, "panic", panicked)
		}()
		c.Next()
	}
}
