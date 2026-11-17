package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginRateLimits(t *testing.T) {
	now := time.Now()
	var limiter loginLimiter
	for i := 0; i < 30; i++ {
		if !limiter.allowIP("client", now) {
			t.Fatal("early IP throttle")
		}
	}
	if limiter.allowIP("client", now) {
		t.Fatal("IP limit bypassed")
	}
	if !limiter.allowIP("other", now) || !limiter.allowIP("client", now.Add(5*time.Minute)) {
		t.Fatal("unrelated/expired IP blocked")
	}
	for i := 0; i < 10; i++ {
		if !limiter.beginAccount("alice", now) {
			t.Fatal("early account throttle")
		}
		limiter.done()
	}
	if limiter.beginAccount("alice", now) {
		t.Fatal("account limit bypassed")
	}
	for _, user := range []string{"bob", "carol", "dave", "eve"} {
		if !limiter.beginAccount(user, now) {
			t.Fatal("early concurrency throttle")
		}
	}
	if limiter.beginAccount("frank", now) {
		t.Fatal("bcrypt concurrency limit bypassed")
	}
	for i := 0; i < 4; i++ {
		limiter.done()
	}
	if !limiter.beginAccount("frank", now) {
		t.Fatal("verification slot not released")
	}
	limiter.done()
}

func TestLoginThrottlesBeforeDatabase(t *testing.T) {
	a := &application{}
	a.loginLimits.global = loginBucket{count: 120, expires: time.Now().Add(time.Minute)}
	w := httptest.NewRecorder()
	a.api(w, httptest.NewRequest("POST", "/api/login", nil))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("login did not throttle")
	}
}
