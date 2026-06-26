package director

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

type userView struct {
	ID        int64  `json:"id"`
	ISPID     uint32 `json:"ispId"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
	Self      bool   `json:"self"`
}

// apiListUsers lists logins. Director sees all (optionally ?isp=N); an ISP user
// sees only their own tenant's users.
func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	scope := tenantScope(id) // 0 for director → all; own ISP otherwise
	if id.isDirector() {
		scope = parseUint32(r.URL.Query().Get("isp")) // optional filter; 0 = all
	}
	us, err := s.store.ListUsers(r.Context(), scope)
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	out := make([]userView, 0, len(us))
	for _, u := range us {
		out = append(out, userView{ID: u.ID, ISPID: u.ISPID, Email: u.Email, Role: string(u.Role),
			CreatedAt: u.CreatedAt.In(istLoc).Format("2006-01-02 15:04"), Self: u.ID == id.UserID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "isDirector": id.isDirector()})
}

// apiCreateUser adds a login. A director can add a user to any ISP (or another
// director); an ISP user can only add users within their own tenant.
func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var b struct {
		Email, Password, Role string
		ISPID                 uint32
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil {
		s.jsonErr(w, clientErr("bad request"))
		return
	}
	email := strings.ToLower(strings.TrimSpace(b.Email))
	if email == "" || !strings.Contains(email, "@") {
		s.jsonErr(w, clientErr("a valid email is required"))
		return
	}
	if len(b.Password) < 8 {
		s.jsonErr(w, clientErr("password must be at least 8 characters"))
		return
	}
	role := store.RoleISP
	ispID := id.ISPID
	if id.isDirector() {
		if b.Role == "director" {
			role, ispID = store.RoleDirector, 0
		} else {
			ispID = b.ISPID
			if ispID == 0 {
				s.jsonErr(w, clientErr("choose an ISP for this user"))
				return
			}
		}
	}
	if ispID != 0 { // verify the tenant exists
		if _, err := s.store.GetISP(r.Context(), ispID); err != nil {
			s.jsonErr(w, clientErr("unknown ISP"))
			return
		}
	}
	hash, err := HashPassword(b.Password)
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	if _, err := s.store.CreateUser(r.Context(), store.User{ISPID: ispID, Email: email, PasswordHash: hash, Role: role}); err != nil {
		s.jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// apiDeleteUser removes a login (revocation). Cannot remove yourself; an ISP user
// can only remove users in their own tenant.
func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	target, err := s.store.GetUser(r.Context(), int64(parseUint32(r.PathValue("id"))))
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	if target.ID == id.UserID {
		s.jsonErr(w, clientErr("you cannot delete your own account"))
		return
	}
	if !s.canManageUser(id, target) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if err := s.store.DeleteUser(r.Context(), target.ID); err != nil {
		s.jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// apiResetPassword lets an admin set another user's password (no old password).
func (s *Server) apiResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var b struct{ Password string }
	if json.NewDecoder(r.Body).Decode(&b) != nil || len(b.Password) < 8 {
		s.jsonErr(w, clientErr("password must be at least 8 characters"))
		return
	}
	target, err := s.store.GetUser(r.Context(), int64(parseUint32(r.PathValue("id"))))
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	if !s.canManageUser(id, target) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	hash, err := HashPassword(b.Password)
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), target.ID, hash); err != nil {
		s.jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// apiChangeOwnPassword lets any signed-in user rotate their own password.
func (s *Server) apiChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var b struct{ OldPassword, NewPassword string }
	if json.NewDecoder(r.Body).Decode(&b) != nil {
		s.jsonErr(w, clientErr("bad request"))
		return
	}
	if len(b.NewPassword) < 8 {
		s.jsonErr(w, clientErr("new password must be at least 8 characters"))
		return
	}
	me, err := s.store.GetUser(r.Context(), id.UserID)
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	if !VerifyPassword(me.PasswordHash, b.OldPassword) {
		s.jsonErr(w, clientErr("current password is incorrect"))
		return
	}
	hash, err := HashPassword(b.NewPassword)
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	if err := s.store.UpdateUserPassword(r.Context(), id.UserID, hash); err != nil {
		s.jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// canManageUser: a director may manage anyone; an ISP user only users in their
// own tenant (and never a director).
func (s *Server) canManageUser(id Identity, target store.User) bool {
	if id.isDirector() {
		return true
	}
	return target.ISPID == id.ISPID && target.Role == store.RoleISP
}
