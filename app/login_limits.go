package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

type loginBucket struct {
	count   int
	expires time.Time
}
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]loginBucket
	global  loginBucket
	active  int
}

func (l *loginLimiter) take(key string, maximum int, window time.Duration, now time.Time) bool {
	if l.buckets == nil {
		l.buckets = map[string]loginBucket{}
	}
	b := l.buckets[key]
	if !now.Before(b.expires) {
		// Bound unauthenticated memory usage without evicting active limits.
		if len(l.buckets) >= 10000 {
			for k, v := range l.buckets {
				if !now.Before(v.expires) {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= 10000 {
				return false
			}
		}
		b = loginBucket{expires: now.Add(window)}
	}
	if b.count >= maximum {
		return false
	}
	b.count++
	l.buckets[key] = b
	return true
}
func (l *loginLimiter) allowIP(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.global.expires) {
		l.global = loginBucket{expires: now.Add(time.Minute)}
	}
	if l.global.count >= 120 {
		return false
	}
	l.global.count++
	return l.take("ip:"+ip, 30, 5*time.Minute, now)
}
func (l *loginLimiter) beginAccount(username string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= 4 {
		return false
	}
	h := sha256.Sum256([]byte(username))
	if !l.take("account:"+hex.EncodeToString(h[:]), 10, 5*time.Minute, now) {
		return false
	}
	l.active++
	return true
}
func (l *loginLimiter) done() { l.mu.Lock(); l.active--; l.mu.Unlock() }
func loginThrottled(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "300")
	writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
}
