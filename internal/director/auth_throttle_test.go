package director

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func TestLoginThrottle_LocksOutAfterMaxFailures(t *testing.T) {
	throttle := NewLoginThrottle(3, 500*time.Millisecond, 1*time.Second)
	key := "192.0.2.1:test@example.com"

	// 1st failure: not locked
	locked, _ := throttle.RecordFailure(key)
	if locked {
		t.Fatal("expected not locked on 1st failure")
	}

	// 2nd failure: not locked
	locked, _ = throttle.RecordFailure(key)
	if locked {
		t.Fatal("expected not locked on 2nd failure")
	}

	// 3rd failure: locked!
	locked, rem := throttle.RecordFailure(key)
	if !locked || rem <= 0 {
		t.Fatalf("expected locked after 3 failures, got locked=%v rem=%v", locked, rem)
	}

	// Check reports locked
	locked, _ = throttle.Check(key)
	if !locked {
		t.Fatal("check should report locked")
	}

	// Wait for lockout expiry
	time.Sleep(1100 * time.Millisecond)
	locked, _ = throttle.Check(key)
	if locked {
		t.Fatal("expected lockout to have expired")
	}
}

func TestLoginThrottle_SuccessClearsHistory(t *testing.T) {
	throttle := NewLoginThrottle(3, 500*time.Millisecond, 1*time.Second)
	key := "192.0.2.2:user@example.com"

	throttle.RecordFailure(key)
	throttle.RecordFailure(key)
	throttle.RecordSuccess(key)

	// Next failure should be count 1, not 3
	locked, _ := throttle.RecordFailure(key)
	if locked {
		t.Fatal("expected not locked after success reset")
	}
}

func TestHandleLogin_ThrottlesBruteForce(t *testing.T) {
	s, st := testServer(t)
	pw, _ := HashPassword("correct-horse-battery-staple")
	_, err := st.CreateUser(context.Background(), store.User{
		Email: "victim@example.com", PasswordHash: pw, Role: store.RoleDirector,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Try 5 wrong logins
	for i := 0; i < 5; i++ {
		form := url.Values{
			"email":    {"victim@example.com"},
			"password": {"wrong-password"},
		}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "203.0.113.50:12345"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i+1, w.Code)
		}
	}

	// 6th attempt (even with correct password) should be locked out (429 Too Many Requests)
	form := url.Values{
		"email":    {"victim@example.com"},
		"password": {"correct-horse-battery-staple"},
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.50:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on 6th attempt, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Too many failed login attempts") {
		t.Errorf("expected lockout message in body, got: %s", w.Body.String())
	}
}
