package director

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

// Identity is the authenticated principal carried in a signed session cookie.
type Identity struct {
	UserID int64      `json:"u"`
	ISPID  uint32     `json:"i"`
	Role   store.Role `json:"r"`
	Email  string     `json:"e"`
	Exp    int64      `json:"x"` // unix expiry
}

const sessionTTL = 12 * time.Hour

// HashPassword returns a bcrypt hash.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// VerifyPassword reports whether pw matches the bcrypt hash.
func VerifyPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// HashToken returns the hex sha256 of an agent token (tokens are high-entropy,
// so a fast hash is appropriate; only the hash is stored).
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// NewToken returns a fresh random agent token (hex).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// signSession encodes and HMAC-signs an Identity into a cookie value.
func (s *Server) signSession(id Identity) string {
	payload, _ := json.Marshal(id)
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.sessionKey)
	mac.Write([]byte(p))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return p + "." + sig
}

// parseSession verifies and decodes a session cookie value.
func (s *Server) parseSession(v string) (Identity, bool) {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return Identity{}, false
	}
	mac := hmac.New(sha256.New, s.sessionKey)
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return Identity{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Identity{}, false
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return Identity{}, false
	}
	if time.Now().Unix() > id.Exp {
		return Identity{}, false
	}
	return id, true
}

// csrfToken derives a per-session anti-CSRF token (HMAC of the session identity
// with the server key). A cross-site attacker cannot compute it without the
// server key, and it is bound to the specific session.
func (s *Server) csrfToken(id Identity) string {
	mac := hmac.New(sha256.New, s.sessionKey)
	fmt.Fprintf(mac, "csrf|%d|%d", id.UserID, id.Exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validCSRF reports whether the submitted token matches (constant-time).
func (s *Server) validCSRF(id Identity, submitted string) bool {
	return subtle.ConstantTimeCompare([]byte(s.csrfToken(id)), []byte(submitted)) == 1
}

func (id Identity) isDirector() bool { return id.Role == store.RoleDirector }

// IsDirector is the exported form used by templates.
func (id Identity) IsDirector() bool { return id.isDirector() }

// scopeISP returns the isp_id a request is allowed to act on. Directors may pass
// any requested isp (0 = all); ISP users are pinned to their own.
func (id Identity) scopeISP(requested uint32) (uint32, error) {
	if id.isDirector() {
		return requested, nil
	}
	if id.ISPID == 0 {
		return 0, fmt.Errorf("forbidden: no tenant") // fail closed: never let a non-director resolve to global (0)
	}
	if requested != 0 && requested != id.ISPID {
		return 0, fmt.Errorf("forbidden: cross-tenant access")
	}
	return id.ISPID, nil
}
