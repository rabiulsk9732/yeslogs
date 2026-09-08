package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrPolicyInUse = errors.New("policy is referenced by devices; reassign them before renaming or deleting")

func samePolicy(a, b CapturePolicy) bool {
	return a.ID == b.ID && a.ISPID == b.ISPID && a.Name == b.Name && a.SkipDNS == b.SkipDNS && a.SkipPrivate == b.SkipPrivate && a.SkipZero == b.SkipZero && a.CreatedAt.Equal(b.CreatedAt)
}
func policyOverlap(a, b CapturePolicy) bool {
	return a.ID != b.ID && (a.ISPID == 0 || b.ISPID == 0 || a.ISPID == b.ISPID) && strings.EqualFold(a.Name, b.Name)
}
func policyReference(p CapturePolicy, d Device) bool {
	return (p.ISPID == 0 || p.ISPID == d.ISPID) && strings.EqualFold(p.Name, d.CapturePolicy)
}

// SavePolicy atomically checks a content snapshot and changes only the preset.
// A nil replacement deletes; a nil expected creates. Device rules are untouched.
func (m *MemStore) SavePolicy(_ context.Context, expected, replacement *CapturePolicy) (CapturePolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var old CapturePolicy
	if expected != nil {
		var ok bool
		old, ok = m.policies[expected.ID]
		if !ok {
			return old, ErrNotFound
		}
		if !samePolicy(old, *expected) {
			return old, ErrConflict
		}
		if replacement == nil || replacement.Name != old.Name {
			for _, d := range m.devices {
				if policyReference(old, d) {
					return old, ErrPolicyInUse
				}
			}
		}
	}
	if replacement == nil {
		delete(m.policies, old.ID)
		return old, nil
	}
	p := *replacement
	if expected != nil {
		p.ID = old.ID
		p.ISPID = old.ISPID
		p.CreatedAt = old.CreatedAt
	}
	for _, other := range m.policies {
		if policyOverlap(p, other) {
			return p, ErrDuplicate
		}
	}
	if expected == nil {
		if p.ISPID != 0 {
			if _, ok := m.isps[p.ISPID]; !ok {
				return p, ErrNotFound
			}
		}
		m.nextPol++
		p.ID = m.nextPol
		p.CreatedAt = time.Now().UTC()
	}
	if m.policies == nil {
		m.policies = map[int64]CapturePolicy{}
	}
	m.policies[p.ID] = p
	return p, nil
}

func (s *MySQLStore) SavePolicy(ctx context.Context, expected, replacement *CapturePolicy) (CapturePolicy, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CapturePolicy{}, err
	}
	defer tx.Rollback()
	// Lock the policy namespace, including global/tenant name collisions.
	rows, err := tx.QueryContext(ctx, `SELECT id,isp_id,name,skip_dns,skip_private,skip_zero,created_at FROM capture_policies ORDER BY id FOR UPDATE`)
	if err != nil {
		return CapturePolicy{}, err
	}
	var policies []CapturePolicy
	for rows.Next() {
		var p CapturePolicy
		if err = rows.Scan(&p.ID, &p.ISPID, &p.Name, &p.SkipDNS, &p.SkipPrivate, &p.SkipZero, &p.CreatedAt); err != nil {
			rows.Close()
			return p, err
		}
		policies = append(policies, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return CapturePolicy{}, err
	}
	var old CapturePolicy
	if expected != nil {
		for _, p := range policies {
			if p.ID == expected.ID {
				old = p
				break
			}
		}
		if old.ID == 0 {
			return old, ErrNotFound
		}
		if !samePolicy(old, *expected) {
			return old, ErrConflict
		}
		if replacement == nil || replacement.Name != old.Name {
			var count int
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE capture_policy=? AND (?=0 OR isp_id=?)`, old.Name, old.ISPID, old.ISPID).Scan(&count)
			if err != nil {
				return old, err
			}
			if count > 0 {
				return old, ErrPolicyInUse
			}
		}
	}
	if replacement == nil {
		if _, err = tx.ExecContext(ctx, `DELETE FROM capture_policies WHERE id=?`, old.ID); err != nil {
			return old, err
		}
		return old, tx.Commit()
	}
	p := *replacement
	if expected != nil {
		p.ID = old.ID
		p.ISPID = old.ISPID
		p.CreatedAt = old.CreatedAt
	}
	for _, other := range policies {
		if policyOverlap(p, other) {
			return p, ErrDuplicate
		}
	}
	if expected == nil {
		if p.ISPID != 0 {
			var n uint32
			if err = tx.QueryRowContext(ctx, `SELECT id FROM isps WHERE id=?`, p.ISPID).Scan(&n); errors.Is(err, sql.ErrNoRows) {
				return p, ErrNotFound
			} else if err != nil {
				return p, err
			}
		}
		result, e := tx.ExecContext(ctx, `INSERT INTO capture_policies (isp_id,name,skip_dns,skip_private,skip_zero) VALUES (?,?,?,?,?)`, p.ISPID, p.Name, p.SkipDNS, p.SkipPrivate, p.SkipZero)
		err = e
		if err == nil {
			p.ID, _ = result.LastInsertId()
			err = tx.QueryRowContext(ctx, `SELECT created_at FROM capture_policies WHERE id=?`, p.ID).Scan(&p.CreatedAt)
		}
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE capture_policies SET name=?,skip_dns=?,skip_private=?,skip_zero=? WHERE id=?`, p.Name, p.SkipDNS, p.SkipPrivate, p.SkipZero, p.ID)
	}
	if isDuplicate(err) {
		return p, ErrDuplicate
	}
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}
