package director

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// DPStats is a dataplane snapshot supplied by the host (natlog) for the Overview.
type DPStats struct {
	Ingested     uint64 // flows decoded since start
	Skipped      uint64 // flows dropped by skip rules since start
	Inserted     uint64 // flows written to hot storage since start
	ArchiveBytes uint64 // bytes uploaded to S3 archive
	QueueSize    int    // current writer queue depth
	QueueMax     int    // configured queue capacity
	Collectors   int    // connected dataplanes/collectors
	Name         string // local dataplane name
}

// SetStats registers the dataplane stats provider.
func (s *Server) SetStats(f func() DPStats) { s.statsFn = f }

func (s *Server) stats() DPStats {
	if s.statsFn != nil {
		return s.statsFn()
	}
	return DPStats{Collectors: 1}
}

func (r *FlowReader) countToday(ctx context.Context, ispID uint32) uint64 {
	var n uint64
	_ = r.conn.QueryRow(ctx, fmt.Sprintf(`SELECT count() FROM %s.flow_logs WHERE event_date = today()%s`, r.db, ispClause(ispID)), ispArgs(ispID)...).Scan(&n)
	return n
}

type ovCard struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Sub   string `json:"sub"`
	Icon  string `json:"icon"`
	Color string `json:"color"`
	Pct   int    `json:"pct"` // 0 = no gauge
}

// handleOverview returns the Director dashboard cards. ISP users get a
// tenant-scoped subset; the Director sees the full dataplane picture.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	var storedToday, hotBytes uint64
	var activeExporters int
	if s.flows != nil {
		storedToday = s.flows.countToday(ctx, id.ISPID)
		_, hotBytes = s.flows.storageStats(ctx, id.ISPID)
	}
	if devs, err := s.store.ListDevices(ctx, tenantScope(id)); err == nil {
		for _, d := range devs {
			if d.Enabled {
				activeExporters++
			}
		}
	}

	cards := []ovCard{}
	if id.isDirector() {
		st := s.stats()
		qpct := 0
		if st.QueueMax > 0 {
			qpct = int(float64(st.QueueSize) / float64(st.QueueMax) * 100)
		}
		if qpct > 100 {
			qpct = 100
		}
		cards = append(cards,
			ovCard{"Flows Ingested", group(st.Ingested), "decoded · since start", "fa-arrow-down-to-line", "#0077b6", 0},
			ovCard{"Flows Stored Today", group(storedToday), "written to hot storage", "fa-database", "#2a9d8f", 0},
			ovCard{"Flows Skipped", group(st.Skipped), "dropped by skip rules", "fa-filter-circle-xmark", "#7b8794", 0},
			ovCard{"Active Dataplanes", fmt.Sprintf("%d", st.Collectors), "collectors connected", "fa-network-wired", "#00a3c4", 0},
			ovCard{"Active Exporters", fmt.Sprintf("%d", activeExporters), "enabled devices", "fa-server", "#0077b6", 0},
			ovCard{"Hot Storage Used", humanBytes(hotBytes), "ClickHouse on disk", "fa-hard-drive", "#e76f51", 0},
			ovCard{"Archive Uploaded", humanBytes(st.ArchiveBytes), "to S3 cold storage", "fa-box-archive", "#2a9d8f", 0},
			ovCard{"Queue Pressure", fmt.Sprintf("%d%%", qpct), fmt.Sprintf("%s / %s rows", group(uint64(st.QueueSize)), group(uint64(st.QueueMax))), "fa-gauge-high", queueColor(qpct), qpct},
		)
	} else {
		cards = append(cards,
			ovCard{"Flows Stored Today", group(storedToday), "your ISP", "fa-database", "#2a9d8f", 0},
			ovCard{"Active Exporters", fmt.Sprintf("%d", activeExporters), "your enabled devices", "fa-server", "#0077b6", 0},
			ovCard{"Hot Storage Used", humanBytes(hotBytes), "your logged volume", "fa-hard-drive", "#e76f51", 0},
		)
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": cards})
}

func queueColor(p int) string {
	switch {
	case p >= 85:
		return "#e63946"
	case p >= 60:
		return "#ffb703"
	default:
		return "#2a9d8f"
	}
}

// handleDataplanes lists connected dataplanes/collectors (director only). For now
// a single in-process dataplane; agents (remote collectors) appear as they
// register and pull config.
func (s *Server) handleDataplanes(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !id.isDirector() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	st := s.stats()
	qpct := 0
	if st.QueueMax > 0 {
		qpct = int(float64(st.QueueSize) / float64(st.QueueMax) * 100)
	}
	if qpct > 100 {
		qpct = 100
	}
	local := map[string]any{
		"name": orDash(st.Name), "kind": "in-process", "status": "live",
		"ingested": st.Ingested, "inserted": st.Inserted, "skipped": st.Skipped,
		"queuePct": qpct, "uptime": int64(time.Since(s.startedAt).Seconds()),
	}
	dps := []map[string]any{local}
	writeJSON(w, http.StatusOK, map[string]any{"dataplanes": dps})
}
