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
	`CREATE TABLE IF NOT EXISTS query_audit (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		user_email VARCHAR(190) NOT NULL,
		isp_id INT UNSIGNED NOT NULL DEFAULT 0,
		query_ip VARCHAR(45) NOT NULL,
		query_port INT NOT NULL DEFAULT 0,
		query_proto VARCHAR(8) NOT NULL DEFAULT '',
		from_ts DATETIME NULL,
		to_ts DATETIME NULL,
		result_count INT NOT NULL DEFAULT 0,
		case_ref VARCHAR(190) NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_qa_isp (isp_id, created_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS settings (
		section VARCHAR(32) NOT NULL PRIMARY KEY,
		data MEDIUMTEXT NOT NULL,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS capture_policies (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		isp_id INT UNSIGNED NOT NULL DEFAULT 0,
		name VARCHAR(64) NOT NULL,
		skip_dns TINYINT(1) NOT NULL DEFAULT 1,
		skip_private TINYINT(1) NOT NULL DEFAULT 1,
		skip_zero TINYINT(1) NOT NULL DEFAULT 1,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE KEY uq_policy (isp_id, name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`ALTER TABLE devices ADD COLUMN IF NOT EXISTS capture_policy VARCHAR(64) NOT NULL DEFAULT ''`,
	"CREATE TABLE IF NOT EXISTS archived_days (" +
		"`day` DATE NOT NULL PRIMARY KEY," +
		"`objects` INT NOT NULL DEFAULT 0," +
		"`rows` BIGINT NOT NULL DEFAULT 0," +
		"`bytes` BIGINT NOT NULL DEFAULT 0," +
		"`archived_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
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

func (s *MySQLStore) GetUser(ctx context.Context, id int64) (User, error) {
	var u User
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, isp_id, email, password_hash, role, created_at FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.ISPID, &u.Email, &u.PasswordHash, &role, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	u.Role = Role(role)
	return u, err
}

func (s *MySQLStore) ListUsers(ctx context.Context, ispID uint32) ([]User, error) {
	q := `SELECT id, isp_id, email, role, created_at FROM users`
	var args []any
	if ispID != 0 {
		q += ` WHERE isp_id=?`
		args = append(args, ispID)
	}
	q += ` ORDER BY isp_id, email`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var role string
		if err := rows.Scan(&u.ID, &u.ISPID, &u.Email, &role, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Role = Role(role)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *MySQLStore) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, passwordHash, id)
	if err != nil {
		return err
	}
	return rowsAffectedErr(res)
}

func (s *MySQLStore) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	return rowsAffectedErr(res)
}

func (s *MySQLStore) CreateDevice(ctx context.Context, d Device) (Device, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (isp_id, name, exporter_ip, device_id, protocol, profile, capture_policy, enabled, skip_dns, skip_private, skip_zero)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		d.ISPID, d.Name, d.ExporterIP, d.DeviceID, d.Protocol, d.Profile, d.CapturePolicy, d.Enabled, d.SkipDNS, d.SkipPrivate, d.SkipZero)
	if err != nil {
		if isDuplicate(err) {
			return Device{}, ErrDuplicate
		}
		return Device{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetDevice(ctx, id)
}

const deviceCols = `id, isp_id, name, exporter_ip, device_id, protocol, profile, capture_policy, enabled, skip_dns, skip_private, skip_zero, updated_at`

func scanDevice(sc interface{ Scan(...any) error }) (Device, error) {
	var d Device
	err := sc.Scan(&d.ID, &d.ISPID, &d.Name, &d.ExporterIP, &d.DeviceID, &d.Protocol, &d.Profile,
		&d.CapturePolicy, &d.Enabled, &d.SkipDNS, &d.SkipPrivate, &d.SkipZero, &d.UpdatedAt)
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
		`UPDATE devices SET name=?, exporter_ip=?, device_id=?, protocol=?, profile=?, capture_policy=?, enabled=?, skip_dns=?, skip_private=?, skip_zero=? WHERE id=?`,
		d.Name, d.ExporterIP, d.DeviceID, d.Protocol, d.Profile, d.CapturePolicy, d.Enabled, d.SkipDNS, d.SkipPrivate, d.SkipZero, d.ID)
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

func (s *MySQLStore) LogQuery(ctx context.Context, q QueryAudit) (int64, error) {
	var from, to any
	if !q.FromTS.IsZero() {
		from = q.FromTS.UTC()
	}
	if !q.ToTS.IsZero() {
		to = q.ToTS.UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO query_audit (user_email, isp_id, query_ip, query_port, query_proto, from_ts, to_ts, result_count, case_ref)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		q.UserEmail, q.ISPID, q.QueryIP, q.QueryPort, q.QueryProto, from, to, q.ResultCount, q.CaseRef)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (s *MySQLStore) ListQueries(ctx context.Context, ispID uint32, limit int) ([]QueryAudit, error) {
	const cols = `id, user_email, isp_id, query_ip, query_port, query_proto, from_ts, to_ts, result_count, case_ref, created_at`
	var (
		rows *sql.Rows
		err  error
	)
	if ispID == 0 {
		rows, err = s.db.QueryContext(ctx, `SELECT `+cols+` FROM query_audit ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+cols+` FROM query_audit WHERE isp_id=? ORDER BY id DESC LIMIT ?`, ispID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueryAudit
	for rows.Next() {
		var q QueryAudit
		var from, to sql.NullTime
		if err := rows.Scan(&q.ID, &q.UserEmail, &q.ISPID, &q.QueryIP, &q.QueryPort, &q.QueryProto, &from, &to, &q.ResultCount, &q.CaseRef, &q.CreatedAt); err != nil {
			return nil, err
		}
		if from.Valid {
			q.FromTS = from.Time
		}
		if to.Valid {
			q.ToTS = to.Time
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *MySQLStore) GetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT section, data FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sec, data string
		if err := rows.Scan(&sec, &data); err != nil {
			return nil, err
		}
		out[sec] = data
	}
	return out, rows.Err()
}

func (s *MySQLStore) PutSetting(ctx context.Context, section, jsonData string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (section, data) VALUES (?, ?) ON DUPLICATE KEY UPDATE data = VALUES(data)`,
		section, jsonData)
	return err
}

func (s *MySQLStore) CreatePolicy(ctx context.Context, p CapturePolicy) (CapturePolicy, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO capture_policies (isp_id, name, skip_dns, skip_private, skip_zero) VALUES (?,?,?,?,?)`,
		p.ISPID, p.Name, p.SkipDNS, p.SkipPrivate, p.SkipZero)
	if err != nil {
		if isDuplicate(err) {
			return CapturePolicy{}, ErrDuplicate
		}
		return CapturePolicy{}, err
	}
	p.ID, _ = res.LastInsertId()
	return p, nil
}

func (s *MySQLStore) ListPolicies(ctx context.Context, ispID uint32) ([]CapturePolicy, error) {
	const cols = `id, isp_id, name, skip_dns, skip_private, skip_zero, created_at`
	var (
		rows *sql.Rows
		err  error
	)
	if ispID == 0 {
		rows, err = s.db.QueryContext(ctx, `SELECT `+cols+` FROM capture_policies ORDER BY isp_id, name`)
	} else {
		// tenant policies + global (isp_id 0) presets are usable by the tenant.
		rows, err = s.db.QueryContext(ctx, `SELECT `+cols+` FROM capture_policies WHERE isp_id IN (0, ?) ORDER BY name`, ispID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CapturePolicy
	for rows.Next() {
		var p CapturePolicy
		if err := rows.Scan(&p.ID, &p.ISPID, &p.Name, &p.SkipDNS, &p.SkipPrivate, &p.SkipZero, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *MySQLStore) DeletePolicy(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM capture_policies WHERE id=?`, id)
	if err != nil {
		return err
	}
	return rowsAffectedErr(res)
}

func (s *MySQLStore) IsDayArchived(ctx context.Context, day string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM archived_days WHERE `day`=?", day).Scan(&n)
	return n > 0, err
}

func (s *MySQLStore) MarkDayArchived(ctx context.Context, a ArchivedDay) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO archived_days (`day`, `objects`, `rows`, `bytes`) VALUES (?,?,?,?) "+
			"ON DUPLICATE KEY UPDATE `objects`=VALUES(`objects`), `rows`=VALUES(`rows`), `bytes`=VALUES(`bytes`)",
		a.Day, a.Objects, a.Rows, a.Bytes)
	return err
}

func (s *MySQLStore) ListArchivedDays(ctx context.Context, limit int) ([]ArchivedDay, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT DATE_FORMAT(`day`,'%Y-%m-%d'), `objects`, `rows`, `bytes`, `archived_at` FROM archived_days ORDER BY `day` DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArchivedDay
	for rows.Next() {
		var a ArchivedDay
		if err := rows.Scan(&a.Day, &a.Objects, &a.Rows, &a.Bytes, &a.ArchivedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
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
