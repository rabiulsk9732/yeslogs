package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Existing tenants use their oldest ISP login until an explicit profile is saved.
const ispSelect = `SELECT i.id,i.name,i.enabled,i.created_at,
 COALESCE(p.username,''),COALESCE(u.email,''),COALESCE(p.phone,''),COALESCE(u.id,0),COALESCE(p.version,0)
 FROM isps i LEFT JOIN isp_profiles p ON p.isp_id=i.id
 LEFT JOIN users u ON u.id=COALESCE(p.admin_user_id,(SELECT MIN(u0.id) FROM users u0 WHERE u0.isp_id=i.id AND u0.role='isp')) AND u.isp_id=i.id AND u.role='isp'`

func (s *MySQLStore) GetUserByLogin(ctx context.Context, login string) (User, error) {
	if strings.Contains(login, "@") {
		return s.GetUserByEmail(ctx, login)
	}
	var email string
	err := s.db.QueryRowContext(ctx, `SELECT u.email FROM isp_profiles p JOIN users u ON u.id=p.admin_user_id AND u.isp_id=p.isp_id AND u.role='isp' WHERE p.username=?`, login).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return s.GetUserByEmail(ctx, email)
}

func (s *MySQLStore) SaveISPAccount(ctx context.Context, v ISP, hash string) (out ISP, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	defer func() {
		if isDuplicate(err) {
			err = ErrDuplicate
		}
	}()
	if v.ID == 0 {
		v.AdminUserID = 0
		if hash == "" {
			return out, ErrConflict
		}
		res, e := tx.ExecContext(ctx, `INSERT INTO isps(name,enabled) VALUES(?,?)`, v.Name, v.Enabled)
		if e != nil {
			return out, e
		}
		n, _ := res.LastInsertId()
		v.ID = uint32(n)
		v.Version = 1
	} else {
		var id uint32
		if e := tx.QueryRowContext(ctx, `SELECT id FROM isps WHERE id=? FOR UPDATE`, v.ID).Scan(&id); errors.Is(e, sql.ErrNoRows) {
			return out, ErrNotFound
		} else if e != nil {
			return out, e
		}
		var version uint64
		var primary int64
		e := tx.QueryRowContext(ctx, `SELECT version,admin_user_id FROM isp_profiles WHERE isp_id=?`, v.ID).Scan(&version, &primary)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
		if version != v.Version {
			return out, ErrConflict
		}
		if primary == 0 {
			e = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE isp_id=? AND role='isp' ORDER BY id LIMIT 1 FOR UPDATE`, v.ID).Scan(&primary)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return out, e
			}
		}
		// An explicitly linked account may have been removed through the Users page.
		if primary != 0 {
			var exists int
			if e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id=? AND isp_id=? AND role='isp'`, primary, v.ID).Scan(&exists); e != nil {
				return out, e
			}
			if exists == 0 {
				primary = 0
			}
		}
		v.AdminUserID = primary
		v.Version = version + 1
		if _, e = tx.ExecContext(ctx, `UPDATE isps SET name=?,enabled=? WHERE id=?`, v.Name, v.Enabled, v.ID); e != nil {
			return out, e
		}
	}
	if v.AdminUserID == 0 {
		if hash == "" {
			return out, ErrConflict
		}
		res, e := tx.ExecContext(ctx, `INSERT INTO users(isp_id,email,password_hash,role) VALUES(?,?,?,'isp')`, v.ID, v.Email, hash)
		if e != nil {
			return out, e
		}
		v.AdminUserID, _ = res.LastInsertId()
	} else {
		if _, e := tx.ExecContext(ctx, `UPDATE users SET email=? WHERE id=? AND isp_id=? AND role='isp'`, v.Email, v.AdminUserID, v.ID); e != nil {
			return out, e
		}
		if hash != "" {
			if _, e := tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, v.AdminUserID); e != nil {
				return out, e
			}
		}
	}
	// Deliberately avoid ON DUPLICATE KEY UPDATE: a duplicate username must never
	// update the profile belonging to a different tenant.
	var exists int
	if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM isp_profiles WHERE isp_id=?`, v.ID).Scan(&exists); e != nil {
		return out, e
	}
	if exists == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO isp_profiles(isp_id,username,phone,admin_user_id,version) VALUES(?,?,?,?,?)`, v.ID, v.Username, v.Phone, v.AdminUserID, v.Version)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE isp_profiles SET username=?,phone=?,admin_user_id=?,version=? WHERE isp_id=?`, v.Username, v.Phone, v.AdminUserID, v.Version, v.ID)
	}
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return s.GetISP(ctx, v.ID)
}

func (s *MySQLStore) DeleteISPAccount(ctx context.Context, id uint32, version uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled bool
	if err = tx.QueryRowContext(ctx, `SELECT enabled FROM isps WHERE id=? FOR UPDATE`, id).Scan(&enabled); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var current uint64
	err = tx.QueryRowContext(ctx, `SELECT version FROM isp_profiles WHERE isp_id=?`, id).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current != version || enabled {
		return ErrConflict
	}
	// Lock the indexed dependency ranges until commit, including their gaps, so
	// concurrent inserts cannot slip between the dependency check and deletion.
	for _, table := range []string{"devices", "capture_policies"} {
		rows, e := tx.QueryContext(ctx, `SELECT id FROM `+table+` WHERE isp_id=? FOR UPDATE`, id)
		if e != nil {
			return e
		}
		used := rows.Next()
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if used {
			return ErrInUse
		}
	}
	for _, q := range []string{`DELETE FROM users WHERE isp_id=? AND role='isp'`, `DELETE FROM isp_profiles WHERE isp_id=?`, `DELETE FROM isps WHERE id=?`} {
		if _, err = tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	// Flow records, archives and audit history are intentionally never deleted.
	return tx.Commit()
}

func (m *MemStore) GetUserByLogin(ctx context.Context, login string) (User, error) {
	if strings.Contains(login, "@") {
		return m.GetUserByEmail(ctx, login)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, i := range m.isps {
		if strings.EqualFold(i.Username, login) {
			for _, u := range m.users {
				if u.ID == i.AdminUserID && u.ISPID == i.ID && u.Role == RoleISP {
					return u, nil
				}
			}
		}
	}
	return User{}, ErrNotFound
}
func (m *MemStore) ispAccountLocked(v ISP) ISP {
	var chosen User
	for _, u := range m.users {
		if u.ISPID != v.ID || u.Role != RoleISP {
			continue
		}
		if v.AdminUserID != 0 {
			if u.ID == v.AdminUserID {
				chosen = u
				break
			}
		} else if chosen.ID == 0 || u.ID < chosen.ID {
			chosen = u
		}
	}
	v.AdminUserID = chosen.ID
	v.Email = chosen.Email
	return v
}
func (m *MemStore) SaveISPAccount(_ context.Context, v ISP, hash string) (ISP, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.isps[v.ID]
	if v.ID != 0 {
		if !exists {
			return ISP{}, ErrNotFound
		}
		if old.Version != v.Version {
			return ISP{}, ErrConflict
		}
		old = m.ispAccountLocked(old)
		v.AdminUserID = old.AdminUserID
		v.CreatedAt = old.CreatedAt
	} else {
		v.AdminUserID = 0
	}
	if v.AdminUserID == 0 && hash == "" {
		return ISP{}, ErrConflict
	}
	for _, i := range m.isps {
		if i.ID != v.ID && (strings.EqualFold(i.Name, v.Name) || strings.EqualFold(i.Username, v.Username)) {
			return ISP{}, ErrDuplicate
		}
	}
	for _, u := range m.users {
		if u.ID != v.AdminUserID && strings.EqualFold(u.Email, v.Email) {
			return ISP{}, ErrDuplicate
		}
	}
	if v.ID == 0 {
		m.nextISP++
		v.ID = m.nextISP
		v.CreatedAt = time.Now().UTC()
	}
	var u User
	if v.AdminUserID == 0 {
		m.nextUser++
		v.AdminUserID = m.nextUser
		u = User{ID: v.AdminUserID, ISPID: v.ID, Role: RoleISP, CreatedAt: time.Now().UTC()}
	} else {
		for key, user := range m.users {
			if user.ID == v.AdminUserID {
				u = user
				delete(m.users, key)
				break
			}
		}
	}
	u.Email = v.Email
	if hash != "" {
		u.PasswordHash = hash
	}
	m.users[strings.ToLower(u.Email)] = u
	v.Version++
	m.isps[v.ID] = v
	return v, nil
}
func (m *MemStore) DeleteISPAccount(_ context.Context, id uint32, version uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.isps[id]
	if !ok {
		return ErrNotFound
	}
	if v.Version != version || v.Enabled {
		return ErrConflict
	}
	for _, d := range m.devices {
		if d.ISPID == id {
			return ErrInUse
		}
	}
	for _, p := range m.policies {
		if p.ISPID == id {
			return ErrInUse
		}
	}
	for k, u := range m.users {
		if u.ISPID == id && u.Role == RoleISP {
			delete(m.users, k)
		}
	}
	delete(m.isps, id)
	return nil
}
