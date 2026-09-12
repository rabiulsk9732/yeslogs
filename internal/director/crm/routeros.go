package crm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RouterOSConfig targets MikroTik RouterOS v7 REST User Manager session data.
type RouterOSConfig struct {
	Endpoint, Username, Password string
	Timeout                      time.Duration
	HTTPClient                   *http.Client
}
type RouterOSConnector struct {
	cfg    RouterOSConfig
	client *http.Client
}

func NewRouterOSConnector(cfg RouterOSConfig) (*RouterOSConnector, error) {
	u, e := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if e != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("RouterOS endpoint must be an HTTPS URL")
	}
	if cfg.Username == "" {
		return nil, errors.New("RouterOS username is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	return &RouterOSConnector{cfg: cfg, client: c}, nil
}
func (r *RouterOSConnector) Close() error { return nil }
func (r *RouterOSConnector) Test(ctx context.Context, s LookupRequest) (*LookupResult, error) {
	v, e := r.Lookup(ctx, []LookupRequest{s})
	if e != nil {
		return nil, e
	}
	if len(v) == 0 {
		return nil, errors.New("no result")
	}
	return &v[0], nil
}
func (r *RouterOSConnector) Lookup(ctx context.Context, in []LookupRequest) ([]LookupResult, error) {
	out := make([]LookupResult, len(in))
	for i, l := range in {
		out[i] = LookupResult{ReferenceCode: l.ReferenceCode, Status: StatusNotFound}
		q := url.Values{"address": {l.LocalIP}, "at": {l.EventTime.UTC().Format(time.RFC3339)}}
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.Endpoint+"/rest/user-manager/session?"+q.Encode(), nil)
		if e != nil {
			return nil, e
		}
		req.SetBasicAuth(r.cfg.Username, r.cfg.Password)
		req.Header.Set("Accept", "application/json")
		resp, e := r.client.Do(req)
		if e != nil {
			out[i].Status = StatusError
			out[i].ErrorMessage = e.Error()
			continue
		}
		var rows []struct {
			User             string `json:"user"`
			Username         string `json:"username"`
			Address          string `json:"address"`
			CallingStationID string `json:"calling-station-id"`
			SessionID        string `json:"session-id"`
			NASIPAddress     string `json:"nas-ip-address"`
		}
		e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&rows)
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 {
			out[i].Status = StatusError
			continue
		}
		if len(rows) > 1 {
			out[i].Status = StatusAmbiguous
			continue
		}
		if len(rows) == 1 {
			u := rows[0].Username
			if u == "" {
				u = rows[0].User
			}
			out[i].Status = StatusMatched
			out[i].Subscriber.Username = u
			out[i].Session = SessionInfo{AcctSessionID: rows[0].SessionID, CallingStationID: rows[0].CallingStationID, NASIPAddress: rows[0].NASIPAddress, FramedIPAddress: rows[0].Address}
		}
	}
	return out, nil
}
