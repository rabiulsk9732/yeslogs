package director

import (
	"context"
	"fmt"
	"net/http"
)

// ManagementFlowHandler adds search/report reads to the management gateway.
// Collection, runtime settings, archive jobs and device operations remain with
// the existing upstream. Call InitSettings with the collector's S3 bootstrap
// defaults before constructing this handler; persisted settings take precedence.
func (s *Server) ManagementFlowHandler(upstream string) (http.Handler, error) {
	if s.flows == nil {
		return nil, fmt.Errorf("flow_reads requires an available ClickHouse reader")
	}
	return s.managementHandler(upstream, true)
}

// Refresh the DB-backed read configuration without invoking the collector's
// applier. A settings read failure must not silently disable S3 or enrichment.
func (s *Server) refreshFlowReadSettings(ctx context.Context) error {
	raw, err := s.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	s.applyPersisted(raw)
	c := s.CurrentSettings().S3
	cold := ColdS3{}
	if c.Enabled && c.Bucket != "" {
		cold = ColdS3{Endpoint: c.Endpoint, Bucket: c.Bucket, Prefix: c.PathPrefix,
			AccessKey: c.AccessKey, SecretKey: c.SecretKey, Region: c.Region, Format: c.ExportFormat}
	}
	s.SetColdSource(cold)
	return nil
}
