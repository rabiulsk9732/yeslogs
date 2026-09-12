package director

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/natflow/natflow-dataplane/internal/director/store"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"unicode/utf8"
)

type userView struct {
	ID          int64  `json:"id"`
	ISPID       uint32 `json:"ispId"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	CreatedAt   string `json:"createdAt"`
	Self        bool   `json:"self"`
	Primary     bool   `json:"primary"`
	Version     string `json:"version"`
	TOTPEnabled bool   `json:"totpEnabled"`
}

func userVersion(u store.User) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%s|%s|%s", u.ID, u.ISPID, u.Email, u.Role, u.PasswordHash))))
}
func (s *Server) userView(r *http.Request, u store.User, id Identity) userView {
	primary := false
	if u.ISPID != 0 {
		if i, e := s.store.GetISP(r.Context(), u.ISPID); e == nil {
			primary = i.AdminUserID == u.ID
		}
	}
	return userView{ID: u.ID, ISPID: u.ISPID, Email: u.Email, Role: string(u.Role), CreatedAt: u.CreatedAt.In(istLoc).Format("2006-01-02 15:04"), Self: u.ID == id.UserID, Primary: primary, Version: userVersion(u), TOTPEnabled: u.TOTPEnabled}
}
func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	scope := tenantScope(id)
	if id.isDirector() {
		scope = parseUint32(r.URL.Query().Get("isp"))
	}
	us, e := s.store.ListUsers(r.Context(), scope)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	out := make([]userView, 0, len(us))
	for _, u := range us {
		full, e := s.store.GetUser(r.Context(), u.ID)
		if e != nil {
			s.jsonErr(w, e)
			return
		}
		out = append(out, s.userView(r, full, id))
	}
	res := map[string]any{"users": out, "isDirector": id.isDirector(), "editableUsers": id.canManage()}
	if id.isDirector() {
		isps, e := s.store.ListISPs(r.Context())
		if e != nil {
			s.jsonErr(w, e)
			return
		}
		res["isps"] = isps
	}
	writeJSON(w, 200, res)
}
func (s *Server) ownedUser(r *http.Request, id Identity) (store.User, error) {
	n, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if e != nil || n < 1 {
		return store.User{}, store.ErrNotFound
	}
	u, e := s.store.GetUser(r.Context(), n)
	if e != nil {
		return u, e
	}
	if !s.canManageUser(id, u) {
		return u, store.ErrNotFound
	}
	return u, nil
}
func (s *Server) apiGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	u, e := s.ownedUser(r, id)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	writeJSON(w, 200, s.userView(r, u, id))
}

type userForm struct {
	Email, Password, ConfirmPassword, Role, Version, OldPassword, NewPassword string
	ISPID                                                                     uint32
}

func userFormRead(w http.ResponseWriter, r *http.Request) (userForm, error) {
	var b userForm
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if e := json.NewDecoder(r.Body).Decode(&b); e != nil {
		return b, clientErr("invalid user form")
	}
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	return b, nil
}
func userPassword(p, c string) string {
	if utf8.RuneCountInString(p) < 8 || len(p) > 72 {
		return "Use at least 8 characters, up to 72 UTF-8 bytes."
	}
	if p != c {
		return "Passwords must match."
	}
	return ""
}
func userFields(w http.ResponseWriter, fields map[string]string) bool {
	if len(fields) == 0 {
		return false
	}
	writeJSON(w, 422, map[string]any{"error": "Check the highlighted fields.", "fields": fields})
	return true
}
func (s *Server) userError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, store.ErrConflict):
		writeJSON(w, 409, map[string]string{"error": "This account changed. Close and reopen it before saving."})
	case errors.Is(e, store.ErrPrimaryUser):
		writeJSON(w, 409, map[string]string{"error": "Manage the primary ISP login through the ISPs page."})
	case errors.Is(e, store.ErrDuplicate):
		writeJSON(w, 409, map[string]any{"error": "Email already exists.", "fields": map[string]string{"Email": "Choose a unique email address."}})
	default:
		s.jsonErr(w, e)
	}
}
func (s *Server) userSave(w http.ResponseWriter, r *http.Request, update bool) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	var old store.User
	var e error
	if update {
		old, e = s.ownedUser(r, id)
		if e != nil {
			s.jsonErr(w, e)
			return
		}
		if s.userView(r, old, id).Primary {
			s.userError(w, store.ErrPrimaryUser)
			return
		}
		if old.ID == id.UserID {
			writeJSON(w, 400, map[string]string{"error": "Use Change my password for your own account."})
			return
		}
	}
	b, e := userFormRead(w, r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	fields := map[string]string{}
	a, e := mail.ParseAddress(b.Email)
	if e != nil || a.Address != b.Email || len(b.Email) > 190 || !strings.Contains(b.Email, ".") {
		fields["Email"] = "Enter a valid email address (up to 190 characters)."
	}
	if !update || b.Password != "" || b.ConfirmPassword != "" {
		if msg := userPassword(b.Password, b.ConfirmPassword); msg != "" {
			fields["Password"] = msg
			fields["ConfirmPassword"] = msg
		}
	}
	role, scope := store.Role(b.Role), b.ISPID
	if update {
		role, scope = old.Role, old.ISPID
		if b.Role != "" && store.Role(b.Role) != role || b.ISPID != scope {
			fields["Role"] = "Account role and ISP cannot change. Create a separate login."
		}
		if b.Version != userVersion(old) {
			s.userError(w, store.ErrConflict)
			return
		}
	}
	if role != store.RoleDirector && role != store.RoleISP && role != store.RoleAnalyst && role != store.RoleAuditor {
		fields["Role"] = "Choose Director, ISP Admin, Analyst, or Auditor."
	}
	if !id.isDirector() {
		if !id.canManage() || role == store.RoleDirector || scope != 0 && scope != id.ISPID {
			writeJSON(w, 403, map[string]string{"error": "Cannot manage users outside your ISP."})
			return
		}
		scope = id.ISPID
	}
	if role == store.RoleDirector {
		scope = 0
	} else if _, e = s.store.GetISP(r.Context(), scope); e != nil {
		fields["ISPID"] = "Choose an existing ISP."
	}
	if userFields(w, fields) {
		return
	}
	hash := old.PasswordHash
	if b.Password != "" {
		hash, e = HashPassword(b.Password)
		if e != nil {
			s.jsonErr(w, e)
			return
		}
	}
	u := store.User{ISPID: scope, Email: b.Email, PasswordHash: hash, Role: role}
	if update {
		u, e = s.store.SaveUser(r.Context(), old, &u)
	} else {
		u, e = s.store.CreateUser(r.Context(), u)
	}
	if e != nil {
		s.userError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": u.ID})
}
func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) { s.userSave(w, r, false) }
func (s *Server) apiUpdateUser(w http.ResponseWriter, r *http.Request) { s.userSave(w, r, true) }
func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	u, e := s.ownedUser(r, id)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if u.ID == id.UserID {
		writeJSON(w, 400, map[string]string{"error": "You cannot delete your own account."})
		return
	}
	b, e := userFormRead(w, r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if b.Email != u.Email {
		userFields(w, map[string]string{"Email": "Enter the account email to confirm."})
		return
	}
	if b.Version != userVersion(u) {
		s.userError(w, store.ErrConflict)
		return
	}
	if _, e = s.store.SaveUser(r.Context(), u, nil); e != nil {
		s.userError(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) apiResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	u, e := s.ownedUser(r, id)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if u.ID == id.UserID {
		writeJSON(w, 400, map[string]string{"error": "Use Change my password for your own account."})
		return
	}
	b, e := userFormRead(w, r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if msg := userPassword(b.Password, b.ConfirmPassword); msg != "" {
		userFields(w, map[string]string{"Password": msg, "ConfirmPassword": msg})
		return
	}
	if b.Version != userVersion(u) {
		s.userError(w, store.ErrConflict)
		return
	}
	next := u
	next.PasswordHash, e = HashPassword(b.Password)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if _, e = s.store.SaveUser(r.Context(), u, &next); e != nil {
		s.userError(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) apiChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	b, e := userFormRead(w, r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if msg := userPassword(b.NewPassword, b.ConfirmPassword); msg != "" {
		userFields(w, map[string]string{"NewPassword": msg, "ConfirmPassword": msg})
		return
	}
	u, e := s.store.GetUser(r.Context(), id.UserID)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if !VerifyPassword(u.PasswordHash, b.OldPassword) {
		userFields(w, map[string]string{"OldPassword": "Current password is incorrect."})
		return
	}
	next := u
	next.PasswordHash, e = HashPassword(b.NewPassword)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if _, e = s.store.SaveUser(r.Context(), u, &next); e != nil {
		s.userError(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) canManageUser(id Identity, target store.User) bool {
	return id.isDirector() || id.Role == store.RoleISP && target.ISPID == id.ISPID && target.Role != store.RoleDirector
}
