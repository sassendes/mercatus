package service

import (
	"context"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

const defaultMaxBuckets = 200_000

type RateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	rate       float64
	burst      float64
	maxBuckets int
}

func NewRateLimiter(ratePerSecond float64, burst int) *RateLimiter {
	return &RateLimiter{buckets: map[string]*bucket{}, rate: ratePerSecond, burst: float64(burst), maxBuckets: defaultMaxBuckets}
}

func (rl *RateLimiter) evictIdle(now time.Time, idle time.Duration) {
	for k, b := range rl.buckets {
		if now.Sub(b.last) > idle {
			delete(rl.buckets, k)
		}
	}
}

func (rl *RateLimiter) Allow(key string) (bool, time.Duration) {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= rl.maxBuckets {
			rl.evictIdle(now, time.Minute)
			if len(rl.buckets) >= rl.maxBuckets {
				rl.buckets = map[string]*bucket{}
			}
		}
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	b.tokens = math.Min(rl.burst, b.tokens+now.Sub(b.last).Seconds()*rl.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / rl.rate * float64(time.Second))
	return false, wait
}

func (rl *RateLimiter) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			rl.mu.Lock()
			rl.evictIdle(now, 10*time.Minute)
			rl.mu.Unlock()
		}
	}
}

func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return host
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := rl.Allow(ClientIP(r))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
