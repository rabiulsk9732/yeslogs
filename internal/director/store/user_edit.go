package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrPrimaryUser = errors.New("primary ISP login must be managed through ISPs")

func sameUser(a, b User) bool {
	return a.ID == b.ID && a.ISPID == b.ISPID && a.Email == b.Email && a.Role == b.Role && a.PasswordHash == b.PasswordHash && a.TOTPSecret == b.TOTPSecret && a.TOTPEnabled == b.TOTPEnabled
}
func (m *MemStore) SaveUser(ctx context.Context, old User, next *User) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found User
	key := ""
	for k, u := range m.users {
		if u.ID == old.ID {
			found = u
			key = k
			break
		}
	}
	if key == "" {
		return found, ErrNotFound
	}
	if !sameUser(found, old) {
		return found, ErrConflict
	}
	if next == nil {
		if found.Role == RoleISP {
			primary := m.isps[found.ISPID].AdminUserID
			if primary == 0 {
				for _, u := range m.users {
					if u.ISPID == found.ISPID && u.Role == RoleISP && (primary == 0 || u.ID < primary) {
						primary = u.ID
					}
				}
			}
			if primary == old.ID {
				return found, ErrPrimaryUser
			}
		}
		delete(m.users, key)
		return found, nil
	}
	n := *next
	n.ID = found.ID
	n.ISPID = found.ISPID
	n.Role = found.Role
	n.CreatedAt = found.CreatedAt
	n.TOTPSecret, n.TOTPEnabled = found.TOTPSecret, found.TOTPEnabled
	if u, ok := m.users[n.Email]; ok && u.ID != n.ID {
		return n, ErrDuplicate
	}
	delete(m.users, key)
	m.users[n.Email] = n
	return n, nil
}
func (s *MySQLStore) SaveUser(ctx context.Context, old User, next *User) (User, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return User{}, e
	}
	defer tx.Rollback()
	var u User
	e = tx.QueryRowContext(ctx, `SELECT id,isp_id,email,password_hash,role,created_at,totp_secret,totp_enabled FROM users WHERE id=? FOR UPDATE`, old.ID).Scan(&u.ID, &u.ISPID, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt, &u.TOTPSecret, &u.TOTPEnabled)
	if errors.Is(e, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if e != nil {
		return u, e
	}
	if !sameUser(u, old) {
		return u, ErrConflict
	}
	if next == nil {
		if u.Role == RoleISP {
			var primary int64
			e = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT NULLIF(admin_user_id,0) FROM isp_profiles WHERE isp_id=?),(SELECT MIN(id) FROM users WHERE isp_id=? AND role='isp'),0)`, u.ISPID, u.ISPID).Scan(&primary)
			if e != nil {
				return u, e
			}
			if primary == u.ID {
				return u, ErrPrimaryUser
			}
		}
		_, e = tx.ExecContext(ctx, `DELETE FROM users WHERE id=?`, u.ID)
	} else {
		u.Email = next.Email
		u.PasswordHash = next.PasswordHash
		_, e = tx.ExecContext(ctx, `UPDATE users SET email=?,password_hash=? WHERE id=?`, u.Email, u.PasswordHash, u.ID)
	}
	if isDuplicate(e) {
		return u, ErrDuplicate
	}
	if e != nil {
		return u, e
	}
	return u, tx.Commit()
}
