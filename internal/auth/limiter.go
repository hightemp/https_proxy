package auth

import (
	"sync"
	"time"
)

type failureRecord struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

type failureLimiter struct {
	mu          sync.Mutex
	maxFailures int
	window      time.Duration
	block       time.Duration
	records     map[string]*failureRecord
	now         func() time.Time
	nextCleanup time.Time
}

func newFailureLimiter(maxFailures int, window, block time.Duration) *failureLimiter {
	if maxFailures <= 0 || window <= 0 || block <= 0 {
		return nil
	}
	return &failureLimiter{
		maxFailures: maxFailures,
		window:      window,
		block:       block,
		records:     make(map[string]*failureRecord),
		now:         time.Now,
	}
}

func (l *failureLimiter) retryAfter(key string) time.Duration {
	if l == nil {
		return 0
	}

	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanup(now)

	record := l.records[key]
	if record == nil || record.blockedUntil.IsZero() {
		return 0
	}
	if !now.Before(record.blockedUntil) {
		delete(l.records, key)
		return 0
	}
	return record.blockedUntil.Sub(now)
}

func (l *failureLimiter) recordFailure(key string) {
	if l == nil {
		return
	}

	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanup(now)

	record := l.records[key]
	if record == nil || now.Sub(record.windowStart) >= l.window {
		record = &failureRecord{windowStart: now}
		l.records[key] = record
	}
	record.failures++
	if record.failures >= l.maxFailures {
		record.blockedUntil = now.Add(l.block)
	}
}

func (l *failureLimiter) reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.records, key)
	l.mu.Unlock()
}

func (l *failureLimiter) cleanup(now time.Time) {
	if now.Before(l.nextCleanup) {
		return
	}
	for key, record := range l.records {
		expires := record.windowStart.Add(l.window)
		if record.blockedUntil.After(expires) {
			expires = record.blockedUntil
		}
		if !now.Before(expires) {
			delete(l.records, key)
		}
	}
	l.nextCleanup = now.Add(l.window)
}
