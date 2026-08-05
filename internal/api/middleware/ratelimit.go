package middleware

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
)

const (
	rateLimiterMaxBuckets = 4096
	rateLimiterIdleEvict  = 10 * time.Minute
)

// RateLimiter is a per-client-IP token bucket. It closes the known gap noted
// in docs/security.md: without it, a runaway caller can hammer the SQL and
// folder-listing endpoints unboundedly.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter(ratePerSecond, burst float64) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSecond,
		burst:   burst,
	}
}

// Middleware returns 429 RATE_LIMITED when the client exceeds its budget.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			key = r.RemoteAddr
		}
		if !l.allow(key, time.Now()) {
			respond.Error(w, http.StatusTooManyRequests, "Too many requests", "RATE_LIMITED", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *RateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= rateLimiterMaxBuckets {
			l.evictIdleLocked(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *RateLimiter) evictIdleLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > rateLimiterIdleEvict {
			delete(l.buckets, k)
		}
	}
}
