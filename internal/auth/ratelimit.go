package auth

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Default login rate limits.
//
// These are not optional. HashPassword allocates 19 MiB per call on a box with
// roughly 950 MB usable, so an unthrottled login route is a memory DoS that
// takes one line of shell to trigger.
//
// Two independent buckets, because they stop different attacks: per-IP stops
// one machine hammering the endpoint, per-username stops a slow distributed
// guess against a specific account.
//
// They are overridable because an end-to-end suite logs in far more often than
// a human ever would, and it hits both buckets within a minute. The defaults
// stay strict and production never overrides them; only the test environment
// does. Loosening the shipped values instead would have been the wrong fix to
// the same problem.
const (
	DefaultPerIPPerMinute     = 5
	DefaultPerUsernamePerHour = 10

	limiterIdleLifetime = 2 * time.Hour
)

// Limits configures the login rate limiters.
type Limits struct {
	PerIPPerMinute     int
	PerUsernamePerHour int
}

// DefaultLimits returns the production values.
func DefaultLimits() Limits {
	return Limits{
		PerIPPerMinute:     DefaultPerIPPerMinute,
		PerUsernamePerHour: DefaultPerUsernamePerHour,
	}
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// loginLimiter holds one token bucket per key, evicting idle ones.
//
// A plain map would grow without bound: every distinct IP that ever probes the
// endpoint would allocate a limiter that is never freed, which on this box
// matters.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	limit   rate.Limit
	burst   int
	lastGC  time.Time
}

func newLoginLimiter(every time.Duration, burst int) *loginLimiter {
	return &loginLimiter{
		buckets: make(map[string]*bucket),
		limit:   rate.Every(every),
		burst:   burst,
		lastGC:  time.Now(),
	}
}

// allow reports whether the key may proceed, consuming one token if so.
func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// Amortized cleanup: cheaper and simpler than a background goroutine, and
	// there is no separate lifecycle to shut down.
	if now.Sub(l.lastGC) > limiterIdleLifetime {
		for k, b := range l.buckets {
			if now.Sub(b.lastSeen) > limiterIdleLifetime {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}
	b.lastSeen = now

	return b.limiter.Allow()
}
