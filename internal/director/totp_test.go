package director

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func TestTOTPRFC6238(t *testing.T) { // RFC 6238 SHA-1 vector uses 8 digits; low 6 are 287082.
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	got, e := TOTPCode(secret, time.Unix(59, 0))
	if e != nil || got != "287082" {
		t.Fatalf("%q %v", got, e)
	}
	if !VerifyTOTP(secret, got, time.Unix(59, 0)) {
		t.Fatal("verification failed")
	}
}

func TestTOTPSecretEncryptionRoundTrip(t *testing.T) {
	s, _ := testServer(t)
	const secret = "JBSWY3DPEHPK3PXP"
	sealed, err := s.sealTOTPSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if sealed == secret || !strings.HasPrefix(sealed, totpSecretPrefix) {
		t.Fatalf("TOTP secret was not encrypted at rest: %q", sealed)
	}
	opened, err := s.openTOTPSecret(sealed)
	if err != nil || opened != secret {
		t.Fatalf("round trip failed: %q %v", opened, err)
	}
	if _, err := s.openTOTPSecret(sealed + "x"); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestTOTPSetupStoresEncryptedAndCannotReplaceEnabledKey(t *testing.T) {
	s, st := testServer(t)
	user, err := st.CreateUser(context.Background(), store.User{Email: "totp@example.invalid", Role: store.RoleAnalyst})
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{UserID: user.ID, ISPID: 1, Role: store.RoleAnalyst, Email: user.Email, Exp: time.Now().Add(time.Hour).Unix()}
	w := caseRequest(t, s, http.MethodPost, "/api/v1/account/totp/setup", id, map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	var setup struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &setup); err != nil || setup.Secret == "" {
		t.Fatalf("invalid setup response: %v %s", err, w.Body.String())
	}
	stored, _ := st.GetUser(context.Background(), user.ID)
	if stored.TOTPSecret == setup.Secret || !strings.HasPrefix(stored.TOTPSecret, totpSecretPrefix) || stored.TOTPEnabled {
		t.Fatalf("pending key not protected: %+v", stored)
	}
	code, _ := TOTPCode(setup.Secret, time.Now())
	w = caseRequest(t, s, http.MethodPost, "/api/v1/account/totp/confirm", id, map[string]any{"code": code})
	if w.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", w.Code, w.Body.String())
	}
	w = caseRequest(t, s, http.MethodPost, "/api/v1/account/totp/setup", id, map[string]any{})
	if w.Code != http.StatusConflict {
		t.Fatalf("enabled key was replaceable without disable proof: %d %s", w.Code, w.Body.String())
	}
}
