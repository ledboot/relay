package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

type App struct {
	db             *sql.DB
	provider       Provider
	admin, secret  string
	maxRetries     int
	limitPerMin    int
	keyLimitPerMin int
	limiter        *limiter
}
type bucket struct {
	count int
	at    time.Time
}

const (
	// window is the rate limit accounting period.
	window = time.Minute
	// maxIdentities caps the limiter table; see limiter.max.
	maxIdentities = 50000
	// maxBodyBytes mirrors the body limit enforced by decode.
	maxBodyBytes = 65536
)

// limiter holds per-identity request counters. Identities are the CD Key for the user
// endpoints and the client address for everything else, so one busy customer cannot spend
// another customer's budget.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]bucket
	pruned  time.Time
	// max caps the tracked identities. A new identity beyond it is charged to the coarse
	// bucket instead, so a spray of random CD Keys can neither grow the map without bound
	// nor skip throttling.
	max int
}

func newLimiter(max int) *limiter {
	return &limiter{buckets: map[string]bucket{}, max: max}
}

// allow charges one request to id and reports whether it stays within limit. When the table
// is full and id is new, the request is charged to fallback with fallbackLimit instead.
func (l *limiter) allow(id string, limit int, fallback string, fallbackLimit int) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, seen := l.buckets[id]; !seen && len(l.buckets) >= l.max {
		id, limit = fallback, fallbackLimit
	}
	if now.Sub(l.pruned) >= window {
		for k, v := range l.buckets {
			if now.Sub(v.at) >= window {
				delete(l.buckets, k)
			}
		}
		l.pruned = now
	}
	b := l.buckets[id]
	if now.Sub(b.at) >= window {
		b = bucket{at: now}
	}
	b.count++
	l.buckets[id] = b
	return b.count <= limit
}

// withinLimit reports whether a request is within its rate limit budget. The user endpoints
// are bucketed by CD Key so every customer gets an independent budget no matter how many of
// them share one address (mobile carriers put many customers behind one IP). Everything
// else, including a request whose key cannot be read, uses the coarse client budget.
func (a *App) withinLimit(r *http.Request) bool {
	ip := r.Header.Get("X-Client-IP")
	if ip == "" {
		ip = "direct"
	}
	coarse := "ip:" + ip
	id, limit := coarse, a.limitPerMin
	if r.Method == "POST" && (r.URL.Path == "/api/redeem" || r.URL.Path == "/api/status") {
		if key := requestKey(r); key != "" {
			id, limit = "cdk:"+digest(key), a.keyLimitPerMin
		}
	}
	return a.limiter.allow(id, limit, coarse, a.limitPerMin)
}

// requestKey peeks at the CD Key in a user request body and restores the body so the handler
// can decode it again. io.LimitReader is used rather than http.MaxBytesReader so an oversized
// body is rejected by the handler instead of writing a response from the limiter.
func requestKey(r *http.Request) string {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	var b struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return ""
	}
	return strings.TrimSpace(b.Key)
}
type Key struct {
	ID             int64  `json:"id"`
	Hint           string `json:"hint"`
	Service        string `json:"service"`
	State          string `json:"state"`
	VerificationID string `json:"-"`
	Phone          string `json:"phone"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	EndsAt         string `json:"endsAt"`
	CreatedAt      string `json:"createdAt"`
	// Retries counts completed retry purchases. Internal only: it decides the
	// idempotency key, callers get the outcome through state and error codes.
	Retries int `json:"-"`
}

const columns = "id,hint,service,state,verification_id,phone,code,message,ends_at,created_at,retries"

func scan(s interface{ Scan(...any) error }) (Key, error) {
	var k Key
	e := s.Scan(&k.ID, &k.Hint, &k.Service, &k.State, &k.VerificationID, &k.Phone, &k.Code, &k.Message, &k.EndsAt, &k.CreatedAt, &k.Retries)
	return k, e
}

// purchaseKey keeps the first purchase keyed on the CD Key hash so an interrupted
// redeem resumes instead of buying a second number. A retry must use a different key,
// otherwise upstream idempotency replays the timed-out order and never gives a new
// number. It stays a pure function of (hash,retries) so a crash between the state
// change and the purchase still resumes on the same key.
func purchaseKey(hash string, retries int) string {
	if retries <= 0 {
		return hash
	}
	return digest(fmt.Sprintf("%s:retry:%d", hash, retries))
}
func digest(s string) string {
	h := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(s))))
	return hex.EncodeToString(h[:])
}
func servicePrefix(service string) string {
	var out strings.Builder
	for _, c := range strings.ToUpper(service) {
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out.WriteRune(c)
		}
		if out.Len() == 24 {
			break
		}
	}
	if out.Len() == 0 {
		return "SERVICE"
	}
	return out.String()
}
func validBrand(brand string) bool {
	if len(brand) < 1 || len(brand) > 16 {
		return false
	}
	for _, c := range brand {
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
func randomKey(brand, service string) string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return brand + "-" + servicePrefix(service) + "-" + strings.ToUpper(hex.EncodeToString(b))
}
func openDB(path string) (*sql.DB, error) {
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS keys(id INTEGER PRIMARY KEY,hash TEXT NOT NULL UNIQUE,hint TEXT NOT NULL,service TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'ready',verification_id TEXT NOT NULL DEFAULT '',phone TEXT NOT NULL DEFAULT '',code TEXT NOT NULL DEFAULT '',message TEXT NOT NULL DEFAULT '',ends_at TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')));
 CREATE UNIQUE INDEX IF NOT EXISTS verification_unique ON keys(verification_id) WHERE verification_id<>'';
 CREATE TABLE IF NOT EXISTS events(id TEXT PRIMARY KEY,created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	var hasFullKey int
	if e = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('keys') WHERE name='full_key'").Scan(&hasFullKey); e != nil {
		db.Close()
		return nil, e
	}
	if hasFullKey == 0 {
		if _, e = db.Exec("ALTER TABLE keys ADD COLUMN full_key TEXT NOT NULL DEFAULT ''"); e != nil {
			db.Close()
			return nil, e
		}
	}
	var hasRetries int
	if e = db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('keys') WHERE name='retries'").Scan(&hasRetries); e != nil {
		db.Close()
		return nil, e
	}
	if hasRetries == 0 {
		if _, e = db.Exec("ALTER TABLE keys ADD COLUMN retries INTEGER NOT NULL DEFAULT 0"); e != nil {
			db.Close()
			return nil, e
		}
	}
	return db, nil
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	d := json.NewDecoder(r.Body)
	if d.Decode(v) != nil {
		fail(w, 400, "请求格式不正确")
		return false
	}
	return true
}
func (a *App) routes() http.Handler {
	m := gin.New()
	m.Use(requestLogging())
	_ = m.SetTrustedProxies(nil)
	m.HandleMethodNotAllowed = true
	m.Use(func(c *gin.Context) {
		w, r := c.Writer, c.Request
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// The API port remains private behind the Next.js proxy.
		if r.URL.Path != "/api/webhooks/textverified" && r.URL.Path != "/api/health" && !a.withinLimit(r) {
			c.Abort()
			fail(w, 429, "请求过于频繁，请稍后重试")
			return
		}
		c.Next()
	})
	m.NoRoute(func(c *gin.Context) { fail(c.Writer, 404, "接口不存在") })
	m.NoMethod(func(c *gin.Context) { fail(c.Writer, 405, "请求方法不允许") })
	m.GET("/api/health", func(c *gin.Context) { reply(c.Writer, 200, gin.H{"status": "ok"}) })
	m.POST("/api/redeem", gin.WrapF(a.redeem))
	m.POST("/api/status", gin.WrapF(a.status))
	m.POST("/api/webhooks/textverified", gin.WrapF(a.webhook))
	admin := m.Group("/api/admin", a.adminAuth())
	admin.GET("/services", gin.WrapF(a.adminAPI))
	admin.GET("/keys", gin.WrapF(a.adminAPI))
	admin.POST("/keys", gin.WrapF(a.adminAPI))
	admin.POST("/disable", gin.WrapF(a.adminAPI))
	return m
}

func (a *App) adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		got := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		x, y := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(a.admin))
		if subtle.ConstantTimeCompare(x[:], y[:]) != 1 {
			c.Abort()
			fail(c.Writer, 401, "管理密钥不正确")
			return
		}
		c.Next()
	}
}
func (a *App) adminAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/admin/services":
		s, e := a.provider.Services(r.Context())
		if e != nil {
			fail(w, 502, "无法读取上游平台列表，请检查 API 配置")
			return
		}
		reply(w, 200, s)
	case r.Method == "GET" && r.URL.Path == "/api/admin/keys":
		a.listKeys(w, r)
	case r.Method == "POST" && r.URL.Path == "/api/admin/keys":
		var b struct {
			Service     string
			BrandPrefix string `json:"brandPrefix"`
			Count       int
		}
		if !decode(w, r, &b) {
			return
		}
		b.BrandPrefix = strings.ToUpper(strings.TrimSpace(b.BrandPrefix))
		if b.BrandPrefix == "" {
			b.BrandPrefix = "RELAY"
		}
		if !validBrand(b.BrandPrefix) {
			fail(w, 400, "品牌前缀须为 1–16 位英文字母或数字")
			return
		}
		if b.Count < 1 || b.Count > 100 {
			fail(w, 400, "每次生成 1–100 个")
			return
		}
		services, e := a.provider.Services(r.Context())
		if e != nil {
			fail(w, 502, "无法验证上游平台")
			return
		}
		valid := false
		for _, s := range services {
			if s.Name == b.Service && strings.EqualFold(strings.TrimSpace(s.Capability), "sms") {
				valid = true
			}
		}
		if !valid {
			fail(w, 400, "请选择支持短信的平台")
			return
		}
		tx, e := a.db.Begin()
		if e != nil {
			fail(w, 500, "数据库错误")
			return
		}
		defer tx.Rollback()
		keys := []string{}
		for i := 0; i < b.Count; i++ {
			key := randomKey(b.BrandPrefix, b.Service)
			_, e = tx.Exec("INSERT INTO keys(hash,hint,service,full_key) VALUES(?,?,?,?)", digest(key), b.BrandPrefix+"-"+servicePrefix(b.Service)+"-…"+key[len(key)-6:], b.Service, key)
			if e != nil {
				fail(w, 500, "生成失败")
				return
			}
			keys = append(keys, key)
		}
		if tx.Commit() != nil {
			fail(w, 500, "保存失败")
			return
		}
		reply(w, 201, map[string]any{"keys": keys})
	case r.Method == "POST" && r.URL.Path == "/api/admin/disable":
		var b struct{ ID int64 }
		if !decode(w, r, &b) {
			return
		}
		res, e := a.db.Exec("UPDATE keys SET state='disabled' WHERE id=? AND state='ready'", b.ID)
		if e != nil {
			fail(w, 500, "操作失败")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, 409, "只能禁用未兑换的 CD Key")
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
	default:
		fail(w, 404, "接口不存在")
	}
}
func (a *App) readKey(w http.ResponseWriter, r *http.Request) (Key, string, bool) {
	var b struct{ Key string }
	if !decode(w, r, &b) {
		return Key{}, "", false
	}
	hash := digest(b.Key)
	k, e := scan(a.db.QueryRow("SELECT "+columns+" FROM keys WHERE hash=?", hash))
	if e != nil {
		fail(w, 404, "CD Key 不存在")
		return k, hash, false
	}
	if k.State == "disabled" {
		fail(w, 403, "CD Key 已禁用")
		return k, hash, false
	}
	return k, hash, true
}
func (a *App) redeem(w http.ResponseWriter, r *http.Request) {
	k, hash, ok := a.readKey(w, r)
	if !ok {
		return
	}
	if k.State == "ready" {
		_, e := a.db.Exec("UPDATE keys SET state='allocating' WHERE id=? AND state='ready'", k.ID)
		if e != nil {
			fail(w, 500, "兑换失败")
			return
		}
		k, e = scan(a.db.QueryRow("SELECT "+columns+" FROM keys WHERE id=?", k.ID))
		if e != nil {
			fail(w, 500, "读取订单失败")
			return
		}
		if k.State == "disabled" {
			fail(w, 403, "CD Key 已禁用")
			return
		}
	}
	// A timed-out order never received an SMS, so the CD Key is still good: buy a fresh
	// number for it. Bounded by MAX_RETRIES so a key stuck upstream cannot spend without
	// limit. Cancelled, refunded and reported orders stay manual, as before.
	if k.State == "timed_out" {
		res, e := a.db.Exec("UPDATE keys SET state='allocating',retries=retries+1 WHERE id=? AND state='timed_out' AND retries<?", k.ID, a.maxRetries)
		if e != nil {
			fail(w, 500, "兑换失败")
			return
		}
		n, _ := res.RowsAffected()
		k, e = scan(a.db.QueryRow("SELECT "+columns+" FROM keys WHERE id=?", k.ID))
		if e != nil {
			fail(w, 500, "读取订单失败")
			return
		}
		switch {
		case k.State == "disabled":
			fail(w, 403, "CD Key 已禁用")
			return
		case n == 0 && k.State == "timed_out":
			// 仍停在 timed_out 说明次数已用完；若并发请求已把订单推进到后续状态，
			// 则不报错，直接返回当前订单。
			fail(w, 409, "该 CD Key 的重新兑换次数已用完，请联系管理员处理")
			return
		}
	}
	if k.State == "allocating" { // Stable purchase key survives retries, concurrent requests and process restarts.
		v, e := a.provider.Create(r.Context(), k.Service, purchaseKey(hash, k.Retries))
		if e != nil {
			slog.WarnContext(r.Context(), "purchase pending", "service", k.Service, "key_id", k.ID, "request_id", cRequestID(r), "error", e.Error())
			fail(w, 502, "号码申请暂未完成，请稍后重新兑换")
			return
		}
		_, e = a.db.Exec("UPDATE keys SET verification_id=?,state='waiting' WHERE id=? AND state='allocating'", v.ID, k.ID)
		if e != nil {
			fail(w, 500, "保存订单失败，请重新兑换")
			return
		}
	}
	k, e := a.refresh(r.Context(), k.ID)
	if e != nil {
		fail(w, 502, "订单已保存，上游查询暂时失败，请稍后查询")
		return
	}
	reply(w, 200, k)
}
func (a *App) status(w http.ResponseWriter, r *http.Request) {
	k, _, ok := a.readKey(w, r)
	if !ok {
		return
	}
	k, e := a.refresh(r.Context(), k.ID)
	if e != nil {
		fail(w, 502, "查询暂时失败，请稍后重试")
		return
	}
	reply(w, 200, k)
}
func (a *App) refresh(ctx context.Context, id int64) (Key, error) {
	k, e := scan(a.db.QueryRow("SELECT "+columns+" FROM keys WHERE id=?", id))
	if e != nil || (k.State != "waiting" && k.State != "expired" && !(k.State == "used" && k.Phone == "")) {
		return k, e
	}
	v, msgs, e := a.provider.Get(ctx, k.VerificationID)
	if e != nil {
		return k, e
	}
	_, e = a.db.Exec("UPDATE keys SET phone=?,ends_at=? WHERE id=?", v.Number, v.EndsAt, id)
	if e != nil {
		return k, e
	}
	state := "waiting"
	code, message := "", ""
	for _, m := range msgs {
		if m.Code != "" || m.Content != "" {
			state = "used"
			code = m.Code
			message = m.Content
			break
		}
	}
	if state != "used" {
		switch v.State {
		case "verificationCanceled":
			state = "canceled"
		case "verificationTimedOut":
			state = "timed_out"
		case "verificationRefunded":
			state = "refunded"
		case "verificationReported":
			state = "reported"
		}
	}
	_, e = a.db.Exec("UPDATE keys SET phone=?,ends_at=?,state=?,code=?,message=? WHERE id=? AND state IN ('waiting','expired')", v.Number, v.EndsAt, state, code, message, id)
	if e != nil {
		return k, e
	}
	return scan(a.db.QueryRow("SELECT "+columns+" FROM keys WHERE id=?", id))
}
func (a *App) webhook(w http.ResponseWriter, r *http.Request) {
	if a.secret == "" {
		fail(w, 503, "Webhook 未配置")
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if e != nil {
		fail(w, 400, "请求过大")
		return
	}
	signature := r.Header.Get("X-Webhook-Signature")
	sig, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(signature, "HMAC-SHA512="))
	mac := hmac.New(sha512.New, []byte(a.secret))
	mac.Write(raw)
	if e != nil || !strings.HasPrefix(signature, "HMAC-SHA512=") || !hmac.Equal(sig, mac.Sum(nil)) {
		fail(w, 401, "签名无效")
		return
	}
	var event struct {
		ID             string
		IdempotencyKey string
		Event          string
		Data           SMS
	}
	if json.Unmarshal(raw, &event) != nil || event.ID == "" {
		fail(w, 400, "事件格式错误")
		return
	}
	if event.Event != "v2.sms.received" {
		reply(w, 200, map[string]bool{"ignored": true})
		return
	}
	if event.Data.ReservationID == "" {
		fail(w, 400, "缺少订单标识")
		return
	}
	tx, e := a.db.Begin()
	if e != nil {
		fail(w, 500, "数据库错误")
		return
	}
	defer tx.Rollback()
	eventID := event.IdempotencyKey
	if eventID == "" {
		eventID = event.ID
	}
	res, e := tx.Exec("INSERT OR IGNORE INTO events(id) VALUES(?)", eventID)
	if e != nil {
		fail(w, 500, "事件保存失败")
		return
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		var id int64
		e = tx.QueryRow("SELECT id FROM keys WHERE verification_id=?", event.Data.ReservationID).Scan(&id)
		if e != nil {
			fail(w, 503, "订单尚未同步，请重试")
			return
		}
		if event.Data.Code != "" || event.Data.Content != "" {
			_, e = tx.Exec("UPDATE keys SET state='used',code=?,message=? WHERE id=? AND state IN ('waiting','expired','timed_out','canceled','refunded','reported')", event.Data.Code, event.Data.Content, id)
			if e != nil {
				fail(w, 500, "核销失败")
				return
			}
		}
	}
	if tx.Commit() != nil {
		fail(w, 500, "事件提交失败")
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func main() {
	if err := configureLogging(); err != nil {
		slog.Error("invalid logging configuration", "error", err)
		os.Exit(1)
	}
	gin.SetMode(gin.ReleaseMode)
	admin := os.Getenv("ADMIN_TOKEN")
	if len(admin) < 24 {
		slog.Error("ADMIN_TOKEN must contain at least 24 characters")
		os.Exit(1)
	}
	db, e := openDB(env("DATABASE_PATH", "sms.db"))
	if e != nil {
		slog.Error("backend failure", "error_type", errorType(e))
		os.Exit(1)
	}
	defer db.Close()
	price, e := strconv.ParseFloat(env("TEXTVERIFIED_MAX_PRICE", "5"), 64)
	if e != nil || price <= 0 {
		slog.Error("invalid max price")
		os.Exit(1)
	}
	// Extra paid purchases allowed for one CD Key whose number timed out. 0 disables retry.
	maxRetries, e := strconv.Atoi(env("MAX_RETRIES", "1"))
	if e != nil || maxRetries < 0 {
		slog.Error("invalid max retries")
		os.Exit(1)
	}
	// Budget for admin and keyless requests, and a separate per-CD-Key budget for the user
	// endpoints. They must stay separate: mobile carriers put many customers behind one
	// address, so a shared client budget throttles unrelated customers.
	limitPerMin, e := strconv.Atoi(env("RATE_LIMIT_PER_MIN", "120"))
	if e != nil || limitPerMin < 1 {
		slog.Error("invalid rate limit")
		os.Exit(1)
	}
	keyLimitPerMin, e := strconv.Atoi(env("KEY_RATE_LIMIT_PER_MIN", "120"))
	if e != nil || keyLimitPerMin < 1 {
		slog.Error("invalid key rate limit")
		os.Exit(1)
	}
	base := strings.TrimRight(env("TEXTVERIFIED_BASE_URL", "https://www.textverified.com"), "/")
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	a := &App{db: db, provider: &TV{Base: base, Key: os.Getenv("TEXTVERIFIED_API_KEY"), Username: os.Getenv("TEXTVERIFIED_API_USERNAME"), MaxPrice: price, Client: client}, admin: admin, secret: os.Getenv("TEXTVERIFIED_WEBHOOK_SECRET"), maxRetries: maxRetries, limitPerMin: limitPerMin, keyLimitPerMin: keyLimitPerMin, limiter: newLimiter(maxIdentities)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rows, e := db.Query("SELECT id FROM keys WHERE state IN ('waiting','expired') AND verification_id<>'' ORDER BY id")
				if e != nil {
					continue
				}
				ids := []int64{}
				for rows.Next() {
					var id int64
					if rows.Scan(&id) == nil {
						ids = append(ids, id)
					}
				}
				rows.Close()
				for _, id := range ids {
					if ctx.Err() != nil {
						return
					}
					if _, e := a.refresh(ctx, id); e != nil {
						slog.ErrorContext(ctx, "order sync failed", "key_id", id, "error_type", errorType(e))
					}
				}
			}
		}
	}()
	srv := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(c); err != nil {
			slog.Error("shutdown failed", "error_type", errorType(err))
		}
	}()
	slog.Info("API listening", "address", srv.Addr)
	if e := srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		slog.Error("backend failure", "error_type", errorType(e))
		os.Exit(1)
	}
}
