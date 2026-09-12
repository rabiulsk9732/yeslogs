package director

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// DPStats is a dataplane snapshot supplied by the host (natlog) for the Overview.
type DPStats struct {
	Ingested        uint64 // flows decoded since start
	Skipped         uint64 // flows dropped by skip rules since start
	Inserted        uint64 // flows written to hot storage since start
	ArchiveBytes    uint64 // bytes uploaded to S3 archive
	QueueSize       int    // current writer queue depth
	QueueMax        int    // configured queue capacity
	Collectors      int    // connected dataplanes/collectors
	Name            string // local dataplane name
	KernelUDPDrops  uint64
	NTPHealthy      bool
	NTPConfigured   bool
	DiskUsedPercent float64
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
			ovCard{"Flows Ingested", group(st.Ingested), "decoded · since start", "fa-bolt", tileSky, 0},
			ovCard{"Flows Stored Today", group(storedToday), "written to hot storage", "fa-database", tileGood, 0},
			ovCard{"Flows Skipped", group(st.Skipped), "dropped by skip rules", "fa-filter-circle-xmark", tileMuted, 0},
			ovCard{"Active Dataplanes", fmt.Sprintf("%d", st.Collectors), "collectors connected", "fa-network-wired", tileCyan, 0},
			ovCard{"Active Exporters", fmt.Sprintf("%d", activeExporters), "enabled devices", "fa-server", tileSky, 0},
			ovCard{"Hot Storage Used", humanBytes(hotBytes), "ClickHouse on disk", "fa-hard-drive", tileWarn, 0},
			ovCard{"Archive Uploaded", humanBytes(st.ArchiveBytes), "to S3 cold storage", "fa-box-archive", tileGood, 0},
			ovCard{"Queue Pressure", fmt.Sprintf("%d%%", qpct), fmt.Sprintf("%s / %s rows", group(uint64(st.QueueSize)), group(uint64(st.QueueMax))), "fa-gauge-high", queueColor(qpct), qpct},
			ovCard{"Kernel UDP Drops", group(st.KernelUDPDrops), "receive-buffer overflow · since host boot", "fa-triangle-exclamation", queueColor(func() int {
				if st.KernelUDPDrops > 0 {
					return 100
				}
				return 0
			}()), 0},
			ovCard{"Clock / Disk Guard", func() string {
				if st.NTPConfigured && !st.NTPHealthy {
					return "CLOCK ALERT"
				}
				return fmt.Sprintf("%.1f%% disk", st.DiskUsedPercent)
			}(), "NTP ≤500 ms · disk safety monitored", "fa-shield-halved", func() string {
				if st.NTPConfigured && !st.NTPHealthy || st.DiskUsedPercent >= 90 {
					return tileBad
				}
				if st.DiskUsedPercent >= 85 {
					return tileWarn
				}
				return tileGood
			}(), int(st.DiskUsedPercent)},
		)
	} else {
		cards = append(cards,
			ovCard{"Flows Stored Today", group(storedToday), "your ISP", "fa-database", tileGood, 0},
			ovCard{"Active Exporters", fmt.Sprintf("%d", activeExporters), "your enabled devices", "fa-server", tileSky, 0},
			ovCard{"Logged Volume", humanBytes(hotBytes), "uncompressed traffic logged", "fa-wave-square", tileWarn, 0},
		)
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": cards})
}

// Overview tile accents, taken from the Claude Design artboards. The console
// renders each as a gradient from this colour to a darkened mix of it, so these
// are the LIGHT stop of each tile. The browser only ever receives the hex, so
// these and the console's tokens must be changed together.
const (
	tileSky   = "#2490d8"
	tileGood  = "#12946a"
	tileWarn  = "#cf7f18"
	tileBad   = "#c14343"
	tileCyan  = "#1c9ad0"
	tileMuted = "#5a6d84"
)

func queueColor(p int) string {
	switch {
	case p >= 85:
		return tileBad
	case p >= 60:
		return tileWarn
	default:
		return tileGood
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
