package auth

import (
	"testing"
	"time"
)

// The end-to-end suite raises these limits, because it logs in more often in a
// minute than a human does in a week. These tests exist so that relaxing them
// there does not leave the control itself untested — the whole point of making
// the values configurable was to avoid weakening the shipped defaults.

func TestLimiterAllowsUpToTheBurstThenBlocks(t *testing.T) {
	t.Parallel()

	limiter := newLoginLimiter(time.Minute/5, 5)

	for i := 1; i <= 5; i++ {
		if !limiter.allow("1.2.3.4") {
			t.Fatalf("attempt %d should have been allowed", i)
		}
	}

	// HashPassword allocates 19 MiB per call, so the attempt after the burst is
	// exactly where an unthrottled login route becomes a memory DoS.
	if limiter.allow("1.2.3.4") {
		t.Error("the sixth attempt should have been blocked")
	}
}

func TestLimiterIsPerKey(t *testing.T) {
	t.Parallel()

	limiter := newLoginLimiter(time.Minute/5, 5)

	for i := 0; i < 5; i++ {
		limiter.allow("1.2.3.4")
	}

	// One phone hammering the endpoint must not lock out the other phone.
	if !limiter.allow("5.6.7.8") {
		t.Error("a different key should have its own budget")
	}
}

func TestLimiterRefillsOverTime(t *testing.T) {
	t.Parallel()

	// A fast refill so the test does not sleep for a minute.
	limiter := newLoginLimiter(10*time.Millisecond, 1)

	if !limiter.allow("key") {
		t.Fatal("the first attempt should have been allowed")
	}
	if limiter.allow("key") {
		t.Fatal("the immediate second attempt should have been blocked")
	}

	time.Sleep(30 * time.Millisecond)

	// A locked-out user has to get back in eventually, or a mistyped password
	// during a busy afternoon means no more selling.
	if !limiter.allow("key") {
		t.Error("the budget should refill over time")
	}
}

func TestLimiterEvictsIdleKeys(t *testing.T) {
	t.Parallel()

	limiter := newLoginLimiter(time.Minute/5, 5)

	// Every distinct address that ever probes the endpoint allocates a bucket.
	// Without eviction that map grows without bound, which matters on a box
	// with 1 GB of memory.
	for i := 0; i < 1000; i++ {
		limiter.allow(string(rune(i)))
	}

	limiter.mu.Lock()
	before := len(limiter.buckets)
	// Pretend every bucket has been idle well past the eviction window.
	for _, b := range limiter.buckets {
		b.lastSeen = time.Now().Add(-3 * limiterIdleLifetime)
	}
	limiter.lastGC = time.Now().Add(-3 * limiterIdleLifetime)
	limiter.mu.Unlock()

	limiter.allow("trigger the sweep")

	limiter.mu.Lock()
	after := len(limiter.buckets)
	limiter.mu.Unlock()

	if after >= before {
		t.Errorf("idle buckets were not evicted: %d before, %d after", before, after)
	}
}

func TestDefaultLimitsAreStrict(t *testing.T) {
	t.Parallel()

	// A regression guard with teeth: these are the values that ship. If someone
	// loosens the defaults to make a test suite pass, this fails and says why.
	limits := DefaultLimits()
	if limits.PerIPPerMinute != 5 {
		t.Errorf("per-IP default = %d, want 5", limits.PerIPPerMinute)
	}
	if limits.PerUsernamePerHour != 10 {
		t.Errorf("per-username default = %d, want 10", limits.PerUsernamePerHour)
	}
}
