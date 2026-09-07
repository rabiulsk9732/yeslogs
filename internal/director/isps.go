package director

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

type ispView struct {
	store.ISP
	DeviceCount int
	UserCount   int
}

func (s *Server) ispDirector(w http.ResponseWriter, r *http.Request, mutate bool) bool {
	id, ok := s.authJSON(w, r)
	if !ok {
		return false
	}
	if !id.isDirector() {
		writeJSON(w, 403, map[string]string{"error": "forbidden"})
		return false
	}
	return !mutate || s.csrfOK(w, r, id)
}
func (s *Server) ispViews(r *http.Request) ([]ispView, error) {
	isps, e := s.store.ListISPs(r.Context())
	if e != nil {
		return nil, e
	}
	devs, e := s.store.ListDevices(r.Context(), 0)
	if e != nil {
		return nil, e
	}
	users, e := s.store.ListUsers(r.Context(), 0)
	if e != nil {
		return nil, e
	}
	dc, uc := map[uint32]int{}, map[uint32]int{}
	for _, d := range devs {
		dc[d.ISPID]++
	}
	for _, u := range users {
		if u.Role == store.RoleISP {
			uc[u.ISPID]++
		}
	}
	out := make([]ispView, 0, len(isps))
	for _, i := range isps {
		out = append(out, ispView{i, dc[i.ID], uc[i.ID]})
	}
	return out, nil
}
func (s *Server) apiListISPs(w http.ResponseWriter, r *http.Request) {
	if !s.ispDirector(w, r, false) {
		return
	}
	out, e := s.ispViews(r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"isps": out})
}
func ispID(r *http.Request) (uint32, error) {
	n, e := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if e != nil || n == 0 {
		return 0, clientErr("invalid ISP ID")
	}
	return uint32(n), nil
}
func (s *Server) apiGetISP(w http.ResponseWriter, r *http.Request) {
	if !s.ispDirector(w, r, false) {
		return
	}
	n, e := ispID(r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	out, e := s.ispViews(r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	for _, i := range out {
		if i.ID == n {
			writeJSON(w, 200, i)
			return
		}
	}
	s.jsonErr(w, store.ErrNotFound)
}

type ispForm struct {
	Name, Username, Email, Phone, Password, ConfirmPassword string
	Enabled                                                 *bool
	Version                                                 *uint64
}

var ispUsername = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{2,63}$`)
var ispPhone = regexp.MustCompile(`^\+?[0-9 ()-]{7,25}$`)

func readISPForm(w http.ResponseWriter, r *http.Request) (ispForm, error) {
	var b ispForm
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(&b); e != nil {
		return b, clientErr("invalid form data")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return b, clientErr("invalid form data")
	}
	b.Name = strings.TrimSpace(b.Name)
	b.Username = strings.ToLower(strings.TrimSpace(b.Username))
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	b.Phone = strings.TrimSpace(b.Phone)
	return b, nil
}
func validateISPForm(b ispForm, passwordRequired bool) map[string]string {
	e := map[string]string{}
	if b.Name == "" || utf8.RuneCountInString(b.Name) > 190 {
		e["Name"] = "Enter an ISP name (up to 190 characters)."
	}
	if !ispUsername.MatchString(b.Username) {
		e["Username"] = "Use 3–64 letters, numbers, dots, underscores or hyphens; start with a letter or number."
	}
	email, err := mail.ParseAddress(b.Email)
	if err != nil || email.Address != b.Email || !strings.Contains(b.Email, "@") || len(b.Email) > 190 || !strings.Contains(b.Email[strings.LastIndex(b.Email, "@")+1:], ".") {
		e["Email"] = "Enter a valid email address (up to 190 characters)."
	}
	digits := 0
	for _, c := range b.Phone {
		if c >= '0' && c <= '9' {
			digits++
		}
	}
	if !ispPhone.MatchString(b.Phone) || digits < 7 || digits > 15 {
		e["Phone"] = "Enter a phone number with 7–15 digits and an optional country code."
	}
	if b.Enabled == nil {
		e["Enabled"] = "Choose Active or Disabled."
	}
	if passwordRequired || b.Password != "" || b.ConfirmPassword != "" {
		if utf8.RuneCountInString(b.Password) < 8 || len(b.Password) > 72 {
			e["Password"] = "Use at least 8 characters, up to 72 UTF-8 bytes."
		}
		if b.ConfirmPassword == "" || b.Password != b.ConfirmPassword {
			e["ConfirmPassword"] = "Passwords must match."
		}
	}
	return e
}
func (s *Server) ispSave(w http.ResponseWriter, r *http.Request, update bool) {
	if !s.ispDirector(w, r, true) {
		return
	}
	var old store.ISP
	var e error
	if update {
		n, err := ispID(r)
		if err != nil {
			s.jsonErr(w, err)
			return
		}
		old, e = s.store.GetISP(r.Context(), n)
		if e != nil {
			s.jsonErr(w, e)
			return
		}
	}
	b, e := readISPForm(w, r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	fields := validateISPForm(b, !update || old.AdminUserID == 0)
	if update && b.Version == nil {
		fields["Version"] = "Reload this ISP before saving."
	}
	if len(fields) > 0 {
		writeJSON(w, 422, map[string]any{"error": "Check the highlighted fields.", "fields": fields})
		return
	}
	if update && *b.Version != old.Version {
		s.ispError(w, store.ErrConflict)
		return
	}
	hash := ""
	if b.Password != "" {
		hash, e = HashPassword(b.Password)
		if e != nil {
			s.jsonErr(w, e)
			return
		}
	}
	v := store.ISP{ID: old.ID, Name: b.Name, Username: b.Username, Email: b.Email, Phone: b.Phone, Enabled: *b.Enabled, Version: old.Version}
	saved, e := s.store.SaveISPAccount(r.Context(), v, hash)
	if e != nil {
		s.ispError(w, e)
		return
	}
	writeJSON(w, 200, saved)
}
func (s *Server) apiCreateISP(w http.ResponseWriter, r *http.Request) { s.ispSave(w, r, false) }
func (s *Server) apiUpdateISP(w http.ResponseWriter, r *http.Request) { s.ispSave(w, r, true) }
func (s *Server) apiDeleteISP(w http.ResponseWriter, r *http.Request) {
	if !s.ispDirector(w, r, true) {
		return
	}
	n, e := ispID(r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	var b struct {
		Version *uint64
		Name    string
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if json.NewDecoder(r.Body).Decode(&b) != nil || b.Version == nil {
		s.jsonErr(w, clientErr("confirmation and version are required"))
		return
	}
	v, e := s.store.GetISP(r.Context(), n)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if v.Enabled {
		writeJSON(w, 409, map[string]string{"error": "Disable this ISP before deleting it."})
		return
	}
	if b.Name != v.Name {
		s.jsonErr(w, clientErr("type the ISP name to confirm deletion"))
		return
	}
	if e = s.store.DeleteISPAccount(r.Context(), n, *b.Version); e != nil {
		s.ispError(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) ispError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, store.ErrDuplicate):
		writeJSON(w, 409, map[string]string{"error": "ISP name, username or email already exists. Choose unique values."})
	case errors.Is(e, store.ErrInUse):
		writeJSON(w, 409, map[string]string{"error": "Remove or reassign this ISP's devices and capture policies before deletion."})
	case errors.Is(e, store.ErrConflict):
		writeJSON(w, 409, map[string]string{"error": "This ISP changed. Reload its details before saving or deleting."})
	default:
		s.jsonErr(w, e)
	}
}
