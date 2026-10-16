package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func post(handler http.Handler, path, body, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	if ip != "" {
		r.Header.Set("X-Client-IP", ip)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// 这是本次改动的回归用例：20 个不同客户 + 每个轮询 10 次 = 200 次请求，全部来自同一个
// X-Client-IP（模拟移动运营商 CGNAT，或 TRUST_PROXY=false 时的 shared 值）。
// 按客户端身份限流时第 121 次就会 429，按 CD Key 分桶后每个人都应有独立额度。
func TestRateLimitIsPerCDKey(t *testing.T) {
	a, _, _ := fixture(t)
	keys := make([]string, 20)
	for i := range keys {
		keys[i] = randomKey("RELAY", "openai")
		if _, e := a.db.Exec("INSERT INTO keys(hash,hint,service,state,verification_id) VALUES(?,?,?,?,?)",
			digest(keys[i]), "hint", "openai", "waiting", fmt.Sprintf("v%d", i)); e != nil {
			t.Fatal(e)
		}
	}
	handler := a.routes()
	for _, key := range keys {
		for poll := 0; poll < 10; poll++ {
			if w := post(handler, "/api/status", `{"key":"`+key+`"}`, "shared"); w.Code != 200 {
				t.Fatalf("%s 第 %d 次轮询返回 %d：%s", key[:16], poll+1, w.Code, w.Body.String())
			}
		}
	}
}

func TestRateLimitPerCDKeyBudget(t *testing.T) {
	a, _, key := fixture(t)
	a.keyLimitPerMin = 3
	handler := a.routes()
	for i := 1; i <= 3; i++ {
		if w := post(handler, "/api/status", `{"key":"`+key+`"}`, "1.2.3.4"); w.Code != 200 {
			t.Fatalf("第 %d 次应为 200，实际 %d", i, w.Code)
		}
	}
	w := post(handler, "/api/status", `{"key":"`+key+`"}`, "1.2.3.4")
	if w.Code != 429 || !strings.Contains(w.Body.String(), "请求过于频繁") {
		t.Fatalf("超出单密钥额度应返回 429，实际 %d %s", w.Code, w.Body.String())
	}
	// 别的密钥不受影响：额度是按 CD Key 隔离的
	other := randomKey("RELAY", "openai")
	a.db.Exec("INSERT INTO keys(hash,hint,service,state,verification_id) VALUES(?,?,?,?,?)", digest(other), "hint", "openai", "waiting", "v")
	if w := post(handler, "/api/status", `{"key":"`+other+`"}`, "1.2.3.4"); w.Code != 200 {
		t.Fatalf("另一枚密钥不应被牵连：%d %s", w.Code, w.Body.String())
	}
}

func TestRateLimitFallsBackToClientWithoutKey(t *testing.T) {
	a, _, _ := fixture(t)
	a.limitPerMin = 2
	handler := a.routes()
	for i := 1; i <= 2; i++ {
		// 非法请求体读不到密钥，只能记在客户端身份上
		if w := post(handler, "/api/status", `not-json`, "9.9.9.9"); w.Code == 429 {
			t.Fatalf("第 %d 次不应被限流：%s", i, w.Body.String())
		}
	}
	if w := post(handler, "/api/status", `not-json`, "9.9.9.9"); w.Code != 429 {
		t.Fatalf("无密钥请求应受客户端额度约束，实际 %d", w.Code)
	}
}

// 管理接口不携带 CD Key，必须继续走客户端额度，且不受 KEY_RATE_LIMIT_PER_MIN 影响。
func TestAdminKeepsClientBudget(t *testing.T) {
	a, _, _ := fixture(t)
	a.limitPerMin, a.keyLimitPerMin = 2, 100
	handler := a.routes()
	for i := 1; i <= 2; i++ {
		r := httptest.NewRequest("GET", "/api/admin/keys", nil)
		r.Header.Set("X-Client-IP", "7.7.7.7")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("第 %d 次应为 401（未鉴权），实际 %d", i, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/api/admin/keys", nil)
	r.Header.Set("X-Client-IP", "7.7.7.7")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatalf("管理接口应受客户端额度约束，实际 %d", w.Code)
	}
}

// 身份表写满后，新出现的身份并入客户端桶：既封住内存，也不让随机密钥绕过限流。
func TestKeyTableIsCapped(t *testing.T) {
	a, _, _ := fixture(t)
	a.limiter = newLimiter(2)
	a.limitPerMin, a.keyLimitPerMin = 2, 100
	handler := a.routes()
	for i, key := range []string{"A", "B", "C", "D"} {
		if w := post(handler, "/api/status", `{"key":"`+key+`"}`, "5.5.5.5"); w.Code == 429 {
			t.Fatalf("第 %d 个身份不应被限流：%s", i+1, w.Body.String())
		}
	}
	if w := post(handler, "/api/status", `{"key":"E"}`, "5.5.5.5"); w.Code != 429 {
		t.Fatalf("超出身份表上限后应由客户端桶拦截，实际 %d", w.Code)
	}
	if n := len(a.limiter.buckets); n != 3 {
		t.Fatalf("身份表应只保留 A、B 与客户端桶，实际 %d 项", n)
	}
}

func TestRateLimitWindowResets(t *testing.T) {
	a, _, key := fixture(t)
	a.keyLimitPerMin = 1
	handler := a.routes()
	if w := post(handler, "/api/status", `{"key":"`+key+`"}`, "6.6.6.6"); w.Code != 200 {
		t.Fatalf("首次应为 200：%d", w.Code)
	}
	if w := post(handler, "/api/status", `{"key":"`+key+`"}`, "6.6.6.6"); w.Code != 429 {
		t.Fatalf("同窗口内第二次应 429：%d", w.Code)
	}
	// 把窗口推移到上一分钟，计数必须重置
	id := "cdk:" + digest(key)
	a.limiter.buckets[id] = bucket{count: a.keyLimitPerMin, at: time.Now().Add(-2 * time.Minute)}
	if w := post(handler, "/api/status", `{"key":"`+key+`"}`, "6.6.6.6"); w.Code != 200 {
		t.Fatalf("新窗口应重新放行：%d %s", w.Code, w.Body.String())
	}
}

func TestRequestKeyPeek(t *testing.T) {
	read := func(body string) (string, string) {
		r := httptest.NewRequest("POST", "/api/status", strings.NewReader(body))
		key := requestKey(r)
		rest, e := io.ReadAll(r.Body)
		if e != nil {
			t.Fatal(e)
		}
		return key, string(rest)
	}
	if key, _ := read(`{"key":"RELAY-OPENAI-ABC"}`); key != "RELAY-OPENAI-ABC" {
		t.Fatalf("未取到密钥：%q", key)
	}
	// 前后空白必须被去掉，否则同一枚密钥会产生两个桶
	if key, _ := read("{\"key\":\"  RELAY-OPENAI-ABC  \"}"); key != "RELAY-OPENAI-ABC" {
		t.Fatalf("未去除空白：%q", key)
	}
	for _, body := range []string{``, `not-json`, `{"key":""}`, `{}`, `{"other":"x"}`} {
		if key, _ := read(body); key != "" {
			t.Fatalf("%q 应读不到密钥，实际 %q", body, key)
		}
	}
	// 请求体必须原样还原，否则处理器无法再次解码
	if _, rest := read(`{"key":"K","extra":"kept"}`); !strings.Contains(rest, `"extra":"kept"`) {
		t.Fatalf("请求体未还原：%q", rest)
	}
	// 超大请求体只截断，不由限流器抢先写响应
	big := `{"key":"` + strings.Repeat("A", 70000) + `"}`
	r := httptest.NewRequest("POST", "/api/status", strings.NewReader(big))
	if key := requestKey(r); key != "" {
		t.Fatal("超长请求体不应解析出密钥")
	}
	rest, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	if len(rest) != maxBodyBytes {
		t.Fatalf("应截断到 %d 字节，实际 %d", maxBodyBytes, len(rest))
	}
}

// 超长请求体由处理器返回 400，不能出现限流器与处理器双写响应。
func TestOversizedBodyRejectedByHandler(t *testing.T) {
	a, _, key := fixture(t)
	handler := a.routes()
	big := `{"key":"` + strings.Repeat("A", 70000) + `"}`
	if w := post(handler, "/api/status", big, "8.8.8.8"); w.Code != 400 {
		t.Fatalf("应为 400，实际 %d", w.Code)
	}
	// 限流器不该因此误伤后续正常请求
	if w := post(handler, "/api/status", `{"key":"`+key+`"}`, "8.8.8.8"); w.Code != 200 {
		t.Fatalf("后续请求应为 200，实际 %d", w.Code)
	}
}
