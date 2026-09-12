package crm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// SQLConfig configures direct database access to FreeRADIUS or CRM databases.
type SQLConfig struct {
	Driver            string        `json:"driver"` // default "mysql"
	DSN               string        `json:"dsn"`    // e.g. "user:pass@tcp(127.0.0.1:3306)/radius?parseTime=true"
	RadacctTable      string        `json:"radacctTable"`
	FramedIPCol       string        `json:"framedIpCol"`
	StartTimeCol      string        `json:"startTimeCol"`
	StopTimeCol       string        `json:"stopTimeCol"`
	UsernameCol       string        `json:"usernameCol"`
	CallingStationCol string        `json:"callingStationCol"`
	Timeout           time.Duration `json:"timeout"`
	MaxConns          int           `json:"maxConns"`
}

// SQLConnector directly queries a RADIUS database.
type SQLConnector struct {
	cfg   SQLConfig
	db    *sql.DB
	stmt  *sql.Stmt
	mu    sync.RWMutex
	query string
}

// NewSQLConnector creates a new direct RADIUS SQL connector.
func NewSQLConnector(cfg SQLConfig) (*SQLConnector, error) {
	driver := cfg.Driver
	if driver == "" {
		driver = "mysql"
	}
	if cfg.DSN == "" {
		return nil, errors.New("database DSN is required")
	}

	table := firstNonEmpty(cfg.RadacctTable, "radacct")
	ipCol := firstNonEmpty(cfg.FramedIPCol, "framedipaddress")
	startCol := firstNonEmpty(cfg.StartTimeCol, "acctstarttime")
	stopCol := firstNonEmpty(cfg.StopTimeCol, "acctstoptime")
	userCol := firstNonEmpty(cfg.UsernameCol, "username")
	macCol := firstNonEmpty(cfg.CallingStationCol, "callingstationid")

	query := fmt.Sprintf(
		"SELECT %s, %s FROM %s WHERE %s = ? AND %s <= ? AND (%s IS NULL OR %s = '0000-00-00 00:00:00' OR %s >= ?) ORDER BY %s DESC LIMIT 1",
		userCol, macCol, table, ipCol, startCol, stopCol, stopCol, stopCol, startCol,
	)

	db, err := sql.Open(driver, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	maxConns := cfg.MaxConns
	if maxConns <= 0 {
		maxConns = 10
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(10 * time.Minute)

	return &SQLConnector{
		cfg:   cfg,
		db:    db,
		query: query,
	}, nil
}

// Close closes the underlying SQL database pool.
func (s *SQLConnector) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// Test checks if the database is reachable.
func (s *SQLConnector) Test(ctx context.Context, sample LookupRequest) (*LookupResult, error) {
	if err := s.db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database ping failed: %w", err)
	}
	results, err := s.Lookup(ctx, []LookupRequest{sample})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, errors.New("no result returned")
	}
	return &results[0], nil
}

// Lookup queries the database for each lookup request.
func (s *SQLConnector) Lookup(ctx context.Context, lookups []LookupRequest) ([]LookupResult, error) {
	if len(lookups) == 0 {
		return nil, nil
	}

	timeout := s.cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	results := make([]LookupResult, len(lookups))

	// Execute queries with controlled concurrency
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup

	for i, req := range lookups {
		wg.Add(1)
		go func(idx int, l LookupRequest) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[idx] = LookupResult{
					ReferenceCode: l.ReferenceCode,
					Status:        StatusTimeout,
					ErrorMessage:  ctx.Err().Error(),
				}
				return
			}

			var username, callingStation sql.NullString
			eventTimeStr := l.EventTime.UTC().Format("2006-01-02 15:04:05")

			row := s.db.QueryRowContext(ctx, s.query, l.LocalIP, eventTimeStr, eventTimeStr)
			err := row.Scan(&username, &callingStation)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					results[idx] = LookupResult{
						ReferenceCode: l.ReferenceCode,
						Status:        StatusNotFound,
					}
				} else {
					results[idx] = LookupResult{
						ReferenceCode: l.ReferenceCode,
						Status:        StatusError,
						ErrorMessage:  err.Error(),
					}
				}
				return
			}

			results[idx] = LookupResult{
				ReferenceCode: l.ReferenceCode,
				Status:        StatusMatched,
				Subscriber: SubscriberInfo{
					Username: username.String,
				},
				Session: SessionInfo{
					CallingStationID: callingStation.String,
					FramedIPAddress:  l.LocalIP,
				},
			}
		}(i, req)
	}

	wg.Wait()
	return results, nil
}
