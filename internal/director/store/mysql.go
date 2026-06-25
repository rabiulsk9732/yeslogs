package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQLStore is the MariaDB/MySQL-backed Store.
type MySQLStore struct {
	db *sql.DB
}

// OpenMySQL opens (and pings) a MariaDB/MySQL connection from a DSN, e.g.
// "user:pass@tcp(127.0.0.1:3306)/natflow_cp?parseTime=true&loc=UTC".
func OpenMySQL(dsn string) (*MySQLStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &MySQLStore{db: db}, nil
}

func (s *MySQLStore) Close() error { return s.db.Close() }

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS isps (
		id INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		name VARCHAR(190) NOT NULL UNIQUE,
		enabled TINYINT(1) NOT NULL DEFAULT 1,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS users (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		isp_id INT UNSIGNED NOT NULL DEFAULT 0,
		email VARCHAR(190) NOT NULL UNIQUE,
		password_hash VARCHAR(255) NOT NULL,
		role VARCHAR(16) NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS devices (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		isp_id INT UNSIGNED NOT NULL,
		name VARCHAR(190) NOT NULL,
		exporter_ip VARCHAR(45) NOT NULL UNIQUE,
		device_id INT UNSIGNED NOT NULL,
		protocol VARCHAR(16) NOT NULL DEFAULT 'auto',
		profile VARCHAR(16) NOT NULL DEFAULT 'generic',
		enabled TINYINT(1) NOT NULL DEFAULT 1,
		skip_dns TINYINT(1) NOT NULL DEFAULT 1,
		skip_private TINYINT(1) NOT NULL DEFAULT 1,
		skip_zero TINYINT(1) NOT NULL DEFAULT 1,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		INDEX idx_devices_isp (isp_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS agents (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		name VARCHAR(190) NOT NULL,
		token_hash CHAR(64) NOT NULL UNIQUE,
		last_seen TIMESTAMP NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// Migrate creates the schema if absent.
func (s *MySQLStore) Migrate(ctx context.Context) error {
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func (s *MySQLStore) CreateISP(ctx context.Context, name string) (ISP, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO isps (name) VALUES (?)`, name)
	if err != nil {
		if isDuplicate(err) {
			return ISP{}, ErrDuplicate
		}
		return ISP{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetISP(ctx, uint32(id))
}

func (s *MySQLStore) GetISP(ctx context.Context, id uint32) (ISP, error) {
	var v ISP
	err := s.db.QueryRowContext(ctx, `SELECT id, name, enabled, created_at FROM isps WHERE id=?`, id).
		Scan(&v.ID, &v.Name, &v.Enabled, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ISP{}, ErrNotFound
	}
	return v, err
}

func (s *MySQLStore) ListISPs(ctx context.Context) ([]ISP, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, enabled, created_at FROM isps ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ISP
	for rows.Next() {
		var v ISP
		if err := rows.Scan(&v.ID, &v.Name, &v.Enabled, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *MySQLStore) SetISPEnabled(ctx context.Context, id uint32, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE isps SET enabled=? WHERE id=?`, enabled, id)
	return err
}

func (s *MySQLStore) CreateUser(ctx context.Context, u User) (User, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (isp_id, email, password_hash, role) VALUES (?,?,?,?)`,
		u.ISPID, u.Email, u.PasswordHash, string(u.Role))
	if err != nil {
		if isDuplicate(err) {
			return User{}, ErrDuplicate
		}
		return User{}, err
	}
	u.ID, _ = res.LastInsertId()
	return u, nil
}

func (s *MySQLStore) GetUserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, isp_id, email, password_hash, role, created_at FROM users WHERE email=?`, email).
		Scan(&u.ID, &u.ISPID, &u.Email, &u.PasswordHash, &role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	u.Role = Role(role)
	return u, err
}

func (s *MySQLStore) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *MySQLStore) CreateDevice(ctx context.Context, d Device) (Device, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (isp_id, name, exporter_ip, device_id, protocol, profile, enabled, skip_dns, skip_private, skip_zero)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		d.ISPID, d.Name, d.ExporterIP, d.DeviceID, d.Protocol, d.Profile, d.Enabled, d.SkipDNS, d.SkipPrivate, d.SkipZero)
	if err != nil {
		if isDuplicate(err) {
			return Device{}, ErrDuplicate
		}
		return Device{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetDevice(ctx, id)
}

const deviceCols = `id, isp_id, name, exporter_ip, device_id, protocol, profile, enabled, skip_dns, skip_private, skip_zero, updated_at`

func scanDevice(sc interface{ Scan(...any) error }) (Device, error) {
	var d Device
	err := sc.Scan(&d.ID, &d.ISPID, &d.Name, &d.ExporterIP, &d.DeviceID, &d.Protocol, &d.Profile,
		&d.Enabled, &d.SkipDNS, &d.SkipPrivate, &d.SkipZero, &d.UpdatedAt)
	return d, err
}

func (s *MySQLStore) GetDevice(ctx context.Context, id int64) (Device, error) {
	d, err := scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

func (s *MySQLStore) ListDevices(ctx context.Context, ispID uint32) ([]Device, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if ispID == 0 {
		rows, err = s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY isp_id, id`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE isp_id=? ORDER BY id`, ispID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *MySQLStore) UpdateDevice(ctx context.Context, d Device) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET name=?, exporter_ip=?, device_id=?, protocol=?, profile=?, enabled=?, skip_dns=?, skip_private=?, skip_zero=? WHERE id=?`,
		d.Name, d.ExporterIP, d.DeviceID, d.Protocol, d.Profile, d.Enabled, d.SkipDNS, d.SkipPrivate, d.SkipZero, d.ID)
	if err != nil {
		if isDuplicate(err) {
			return ErrDuplicate
		}
		return err
	}
	return rowsAffectedErr(res)
}

func (s *MySQLStore) DeleteDevice(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id=?`, id)
	if err != nil {
		return err
	}
	return rowsAffectedErr(res)
}

func (s *MySQLStore) CreateAgent(ctx context.Context, name, tokenHash string) (Agent, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO agents (name, token_hash) VALUES (?,?)`, name, tokenHash)
	if err != nil {
		if isDuplicate(err) {
			return Agent{}, ErrDuplicate
		}
		return Agent{}, err
	}
	id, _ := res.LastInsertId()
	return Agent{ID: id, Name: name, TokenHash: tokenHash, CreatedAt: time.Now().UTC()}, nil
}

func (s *MySQLStore) GetAgentByToken(ctx context.Context, tokenHash string) (Agent, error) {
	var a Agent
	var last sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, token_hash, last_seen, created_at FROM agents WHERE token_hash=?`, tokenHash).
		Scan(&a.ID, &a.Name, &a.TokenHash, &last, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if last.Valid {
		a.LastSeen = last.Time
	}
	return a, err
}

func (s *MySQLStore) TouchAgent(ctx context.Context, id int64, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen=? WHERE id=?`, t.UTC(), id)
	return err
}

func rowsAffectedErr(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
