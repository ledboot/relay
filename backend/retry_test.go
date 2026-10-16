package main

import (
	"strings"
	"sync"
	"testing"
)

func TestPurchaseKeyIsStablePerAttempt(t *testing.T) {
	h := digest("RELAY-OPENAI-ABC")
	if purchaseKey(h, 0) != h {
		t.Fatal("首次购买必须继续使用 CD Key 哈希，中断后才能续购而不是重复购买")
	}
	if purchaseKey(h, 1) != purchaseKey(h, 1) {
		t.Fatal("重试幂等键必须可复现，崩溃后才能沿用同一次购买")
	}
	if purchaseKey(h, 1) == purchaseKey(h, 2) || purchaseKey(h, 1) == h {
		t.Fatal("每次重试都需要各自的幂等键")
	}
}

// 超时表示这次没收到短信，CD Key 仍然可用：再次兑换必须买一个新号码，
// 而不是幂等重放那个已经死掉的订单。
func TestTimedOutKeyRetriesWithNewPurchase(t *testing.T) {
	a, f, key := fixture(t)
	p := &stateProvider{fakeProvider: f, state: "verificationTimedOut"}
	a.provider = p
	if w := call(a, "/api/redeem", key); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"timed_out"`) {
		t.Fatalf("前置条件不成立：%d %s", w.Code, w.Body.String())
	}
	if len(f.purchases) != 1 {
		t.Fatalf("首次兑换应只购买一次，实际 %d 次", len(f.purchases))
	}
	p.state = "verificationPending"
	w := call(a, "/api/redeem", key)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"waiting"`) {
		t.Fatalf("重新兑换没有买到新号码：%d %s", w.Code, w.Body.String())
	}
	if _, ok := f.purchases[purchaseKey(digest(key), 1)]; !ok {
		t.Fatal("重试没有使用按次数派生的幂等键")
	}
	if _, ok := f.purchases[digest(key)]; !ok {
		t.Fatal("首次兑换必须继续使用 CD Key 哈希作为幂等键")
	}
	if len(f.purchases) != 2 {
		t.Fatalf("应有两次不同的购买，实际 %d 次", len(f.purchases))
	}
	var id string
	var retries int
	if e := a.db.QueryRow("SELECT reservation_id,retries FROM keys").Scan(&id, &retries); e != nil {
		t.Fatal(e)
	}
	if id != "v2" || retries != 1 {
		t.Fatalf("reservation_id=%q retries=%d，期望 v2 / 1", id, retries)
	}
}

func TestRetryCapStopsPurchasing(t *testing.T) {
	a, f, key := fixture(t)
	p := &stateProvider{fakeProvider: f, state: "verificationTimedOut"}
	a.provider = p
	call(a, "/api/redeem", key)
	p.state = "verificationPending"
	call(a, "/api/redeem", key) // 用掉唯一一次重试
	if len(f.purchases) != 2 {
		t.Fatalf("应有两次购买，实际 %d 次", len(f.purchases))
	}
	a.db.Exec("UPDATE keys SET state='timed_out'")
	w := call(a, "/api/redeem", key)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "重新兑换次数已用完") {
		t.Fatalf("未拦住超出上限的重试：%d %s", w.Code, w.Body.String())
	}
	if len(f.purchases) != 2 {
		t.Fatal("超出上限后仍然购买")
	}
	// CD Key 本身没有被禁用，管理员仍可查询该订单。
	if w := call(a, "/api/status", key); w.Code != 200 {
		t.Fatalf("订单变得不可查询：%d %s", w.Code, w.Body.String())
	}
}

// 两个并发重试请求应该续购同一笔订单，而不是一个成功、另一个误报次数用完，
// 也不能因为竞态多买一个号码。
func TestConcurrentRetryBuysOnce(t *testing.T) {
	a, f, key := fixture(t)
	p := &stateProvider{fakeProvider: f, state: "verificationTimedOut"}
	a.provider = p
	call(a, "/api/redeem", key)
	p.state = "verificationPending"
	a.db.Exec("UPDATE keys SET state='timed_out'")

	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = call(a, "/api/redeem", key).Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("并发重试 %d 返回 %d，应为 200", i, c)
		}
	}
	if len(f.purchases) != 2 {
		t.Fatalf("应为首次 + 一次重试共两次购买，实际 %d 次", len(f.purchases))
	}
}

func TestRetryDisabledWhenMaxRetriesIsZero(t *testing.T) {
	a, f, key := fixture(t)
	a.maxRetries = 0
	a.db.Exec("UPDATE keys SET state='timed_out',reservation_id='v1'")
	if w := call(a, "/api/redeem", key); w.Code != 409 {
		t.Fatalf("MAX_RETRIES=0 应保持人工处理流程：%d %s", w.Code, w.Body.String())
	}
	if len(f.purchases) != 0 {
		t.Fatal("MAX_RETRIES=0 时仍然购买")
	}
}
