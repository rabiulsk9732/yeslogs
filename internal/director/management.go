package director

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
)

// ManagementHandler deploys tenant/account changes independently of the UDP
// receiver. Runtime, flow and device endpoints stay with the original process.
// Public authenticated traffic is checked here against current account state
// before forwarding, so disabled/deleted tenants cannot keep using old cookies.
func (s *Server) ManagementHandler(upstream string) (http.Handler, error) {
	u, e := url.Parse(upstream)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("upstream must be an explicit loopback HTTP origin")
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	local := s.Handler()
	var mutations sync.Mutex
	revision := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// Serialize tenant and dependent inventory/account writes through this
		// gateway, including forwarded writes served by the running collector.
		if r.Method != "GET" && r.Method != "HEAD" && (strings.HasPrefix(p, "/api/v1/isps") || strings.HasPrefix(p, "/api/v1/devices") || strings.HasPrefix(p, "/api/v1/users") || strings.HasPrefix(p, "/api/v1/policies") || strings.HasPrefix(p, "/devices") || strings.HasPrefix(p, "/isps")) {
			mutations.Lock()
			defer mutations.Unlock()
		}
		w.Header().Set("Cache-Control", "no-store")
		if p == "/api/v1/management-version" {
			writeJSON(w, 200, map[string]string{"revision": revision})
			return
		}
		owned := p == "/api/v1/isps" || strings.HasPrefix(p, "/api/v1/isps/") || p == "/api/v1/login" || p == "/api/v1/logout" || p == "/api/v1/me" || p == "/login"
		public := p == "/healthz" || p == "/api/v1/agent/config" || p == "/" || strings.HasPrefix(p, "/assets/")
		if !public && p != "/api/v1/login" && p != "/login" && p != "/api/v1/logout" {
			if _, ok := s.currentIdentity(r); !ok {
				if strings.HasPrefix(p, "/api/") {
					writeJSON(w, 401, map[string]string{"error": "unauthenticated"})
				} else {
					http.Redirect(w, r, "/login", 303)
				}
				return
			}
		}
		// The obsolete HTML ISP mutations must not bypass the required profile fields.
		if p == "/isps" || strings.HasPrefix(p, "/isps/") {
			if r.Method == "GET" {
				http.Redirect(w, r, "/", 303)
			} else {
				writeJSON(w, 400, map[string]string{"error": "Use the ISP management form."})
			}
			return
		}
		if owned {
			local.ServeHTTP(w, r)
		} else {
			proxy.ServeHTTP(w, r)
		}
	}), nil
}
