package director

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // TOTP interoperability profile required by RFC 6238 apps.
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

const totpSecretPrefix = "enc:totp:v1:"

func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}
func TOTPCode(secret string, at time.Time) (string, error) {
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if e != nil {
		return "", e
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(at.Unix()/30))
	h := hmac.New(sha1.New, key)
	h.Write(b[:])
	sum := h.Sum(nil)
	o := sum[len(sum)-1] & 15
	n := (uint32(sum[o])&127)<<24 | uint32(sum[o+1])<<16 | uint32(sum[o+2])<<8 | uint32(sum[o+3])
	return fmt.Sprintf("%06d", n%1000000), nil
}
func VerifyTOTP(secret, code string, at time.Time) bool {
	if len(code) != 6 {
		return false
	}
	for d := -1; d <= 1; d++ {
		want, e := TOTPCode(secret, at.Add(time.Duration(d)*30*time.Second))
		if e == nil && hmac.Equal([]byte(want), []byte(code)) {
			return true
		}
	}
	return false
}

func (s *Server) totpCipher() (cipher.AEAD, error) {
	key := sha256.Sum256(append([]byte("yeslogs-totp-secret-v1:"), s.sessionKey...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Server) sealTOTPSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	aead, err := s.totpCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, []byte(plain), []byte(totpSecretPrefix))
	return totpSecretPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func (s *Server) openTOTPSecret(value string) (string, error) {
	if value == "" || !strings.HasPrefix(value, totpSecretPrefix) {
		return value, nil // backward-compatible migration from an early plaintext value
	}
	aead, err := s.totpCipher()
	if err != nil {
		return "", err
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, totpSecretPrefix))
	if err != nil || len(raw) < aead.NonceSize() {
		return "", fmt.Errorf("invalid encrypted TOTP secret")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(totpSecretPrefix))
	if err != nil {
		return "", fmt.Errorf("could not decrypt TOTP secret")
	}
	return string(plain), nil
}

func (s *Server) verifyUserTOTP(u store.User, code string, at time.Time) bool {
	secret, err := s.openTOTPSecret(u.TOTPSecret)
	return err == nil && VerifyTOTP(secret, code, at)
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	user, err := s.store.GetUser(r.Context(), id.UserID)
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	if user.TOTPEnabled {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "disable existing two-factor authentication before creating a new key"})
		return
	}
	secret, e := NewTOTPSecret()
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	sealed, e := s.sealTOTPSecret(secret)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if e = s.store.UpdateUserTOTP(r.Context(), id.UserID, sealed, false); e != nil {
		s.jsonErr(w, e)
		return
	}
	uri := "otpauth://totp/" + url.PathEscape("YesLogs:"+id.Email) + "?secret=" + secret + "&issuer=YesLogs&algorithm=SHA1&digits=6&period=30"
	writeJSON(w, 200, map[string]string{"secret": secret, "uri": uri})
}
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if e := jsonDecodeLimit(w, r, &body); e != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	u, e := s.store.GetUser(r.Context(), id.UserID)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if !s.verifyUserTOTP(u, body.Code, time.Now()) {
		writeJSON(w, 422, map[string]string{"error": "invalid authenticator code"})
		return
	}
	if e = s.store.UpdateUserTOTP(r.Context(), id.UserID, u.TOTPSecret, true); e != nil {
		s.jsonErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	var body struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if e := jsonDecodeLimit(w, r, &body); e != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	u, e := s.store.GetUser(r.Context(), id.UserID)
	if e != nil || !VerifyPassword(u.PasswordHash, body.Password) || !s.verifyUserTOTP(u, body.Code, time.Now()) {
		writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	if e = s.store.UpdateUserTOTP(r.Context(), id.UserID, "", false); e != nil {
		s.jsonErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func jsonDecodeLimit(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	return json.NewDecoder(r.Body).Decode(v)
}
