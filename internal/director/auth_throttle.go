package director

import (
	"sync"
	"time"
)

type loginAttempt struct {
	count     int
	firstTry  time.Time
	lockoutAt time.Time
}

// LoginThrottle guards login endpoints against brute-force password guessing.
type LoginThrottle struct {
	mu          sync.Mutex
	attempts    map[string]*loginAttempt
	maxAttempts int
	window      time.Duration
	lockoutTime time.Duration
}

// NewLoginThrottle constructs a thread-safe LoginThrottle.
func NewLoginThrottle(maxAttempts int, window, lockout time.Duration) *LoginThrottle {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	if lockout <= 0 {
		lockout = 15 * time.Minute
	}
	return &LoginThrottle{
		attempts:    make(map[string]*loginAttempt),
		maxAttempts: maxAttempts,
		window:      window,
		lockoutTime: lockout,
	}
}

// Check checks whether the key is currently locked out.
func (lt *LoginThrottle) Check(key string) (bool, time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	att, ok := lt.attempts[key]
	if !ok {
		return false, 0
	}
	now := time.Now()
	if !att.lockoutAt.IsZero() {
		if now.Before(att.lockoutAt) {
			return true, att.lockoutAt.Sub(now)
		}
		delete(lt.attempts, key)
		return false, 0
	}
	if now.Sub(att.firstTry) > lt.window {
		delete(lt.attempts, key)
		return false, 0
	}
	return false, 0
}

// RecordFailure increments failed attempt count and applies lockout if threshold reached.
func (lt *LoginThrottle) RecordFailure(key string) (bool, time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	now := time.Now()
	att, ok := lt.attempts[key]
	if !ok || now.Sub(att.firstTry) > lt.window {
		att = &loginAttempt{count: 1, firstTry: now}
		lt.attempts[key] = att
	} else {
		att.count++
	}

	if att.count >= lt.maxAttempts {
		att.lockoutAt = now.Add(lt.lockoutTime)
		return true, lt.lockoutTime
	}

	// Periodic cleanup if map grows
	if len(lt.attempts) > 10000 {
		for k, v := range lt.attempts {
			if (!v.lockoutAt.IsZero() && now.After(v.lockoutAt)) || (v.lockoutAt.IsZero() && now.Sub(v.firstTry) > lt.window) {
				delete(lt.attempts, k)
			}
		}
	}

	return false, 0
}

// RecordSuccess clears failed attempt state after a successful authentication.
func (lt *LoginThrottle) RecordSuccess(key string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	delete(lt.attempts, key)
}
