package director

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// handleSystem reports host + process resource usage (director-only ops view).
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !id.isDirector() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	l1, l5, l15 := readLoadAvg()
	memTotal, memAvail := readMeminfo()
	var fsTotal, fsFree uint64
	var st syscall.Statfs_t
	if syscall.Statfs("/var/lib/clickhouse", &st) == nil || syscall.Statfs("/", &st) == nil {
		fsTotal = st.Blocks * uint64(st.Bsize)
		fsFree = st.Bavail * uint64(st.Bsize)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	resp := map[string]any{
		"cpu": map[string]any{
			"cores": runtime.NumCPU(),
			"load1": l1, "load5": l5, "load15": l15,
			"loadPct": pct(l1, float64(runtime.NumCPU())),
		},
		"memory": map[string]any{
			"total": memTotal, "used": memTotal - memAvail, "human": humanBytes(memTotal - memAvail),
			"totalHuman": humanBytes(memTotal), "pct": pctU(memTotal-memAvail, memTotal),
		},
		"disk": map[string]any{
			"total": fsTotal, "used": fsTotal - fsFree, "human": humanBytes(fsTotal - fsFree),
			"totalHuman": humanBytes(fsTotal), "pct": pctU(fsTotal-fsFree, fsTotal),
		},
		"process": map[string]any{
			"goroutines": runtime.NumGoroutine(),
			"heap":       ms.Alloc, "heapHuman": humanBytes(ms.Alloc),
			"sys": ms.Sys, "sysHuman": humanBytes(ms.Sys),
			"uptime": int64(time.Since(s.startedAt).Seconds()),
		},
	}
	if s.flows != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if sum, err := s.flows.Summary(ctx, 0, 1); err == nil {
			resp["ingest"] = map[string]any{"flowsToday": sum.Rows, "bytes": sum.Bytes, "bytesHuman": humanBytes(sum.Bytes), "devices": sum.Devices}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func readLoadAvg() (l1, l5, l15 float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	f := strings.Fields(string(b))
	if len(f) >= 3 {
		l1, _ = strconv.ParseFloat(f[0], 64)
		l5, _ = strconv.ParseFloat(f[1], 64)
		l15, _ = strconv.ParseFloat(f[2], 64)
	}
	return
}

// readMeminfo returns total + available memory in bytes.
func readMeminfo() (total, avail uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	return
}

func pct(v, of float64) int {
	if of <= 0 {
		return 0
	}
	p := int(v / of * 100)
	if p > 100 {
		p = 100
	}
	return p
}
func pctU(v, of uint64) int {
	if of == 0 {
		return 0
	}
	p := int(v * 100 / of)
	if p > 100 {
		p = 100
	}
	return p
}
