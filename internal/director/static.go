package director

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// The console ships ~1 MB of embedded CSS/JS/fonts. embed.FS files carry no
// modification time, so net/http emits neither Last-Modified nor ETag for them
// and a browser re-fetches every byte on every page load — measured at 957 KB
// per load, which is what made the console feel dead over a long link. Two
// things fix that and neither touches the assets themselves: a strong ETag so
// repeat loads are 304s, and gzip so the first load is roughly a quarter of the
// size.

// assetETag holds a content hash per asset path, computed once at startup.
var (
	assetETagOnce sync.Once
	assetETag     map[string]string
)

func buildAssetETags(root fs.FS) map[string]string {
	m := map[string]string{}
	_ = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := fs.ReadFile(root, p)
		if rerr != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		m["/"+p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return m
}

// cachedAssets adds validation caching to an embedded-file handler. Assets are
// revalidated rather than pinned: a redeploy changes the hash, so a stale asset
// can never outlive its binary, and an unchanged one costs a 304 instead of a
// download.
func cachedAssets(root fs.FS, next http.Handler) http.Handler {
	assetETagOnce.Do(func() { assetETag = buildAssetETags(root) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The mux hands this handler the full path (no StripPrefix), and the keys
		// were built from the same sub-FS the file server reads, so they match as-is.
		if tag, ok := assetETag[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
			for _, want := range strings.Split(r.Header.Get("If-None-Match"), ",") {
				if strings.TrimSpace(want) == tag {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

var gzPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }}

// compressible reports whether a Content-Type is worth gzipping. Fonts and
// images are already compressed; running them through gzip costs CPU and grows
// the payload.
func compressible(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch {
	case strings.HasPrefix(ct, "text/"),
		ct == "application/json",
		ct == "application/javascript",
		ct == "application/xml",
		ct == "image/svg+xml":
		return true
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
	passing bool // true once we know this response is not being compressed
}

// decide runs at the first write, when Content-Type is finally known.
func (g *gzipWriter) decide(status int) {
	g.decided = true
	ct := g.Header().Get("Content-Type")
	// 204/304 carry no body, and an already-encoded body must not be re-encoded.
	if status == http.StatusNoContent || status == http.StatusNotModified ||
		g.Header().Get("Content-Encoding") != "" || !compressible(ct) {
		g.passing = true
		return
	}
	g.Header().Del("Content-Length") // length changes once encoded
	g.Header().Set("Content-Encoding", "gzip")
	g.gz = gzPool.Get().(*gzip.Writer)
	g.gz.Reset(g.ResponseWriter)
}

func (g *gzipWriter) WriteHeader(status int) {
	if !g.decided {
		g.decide(status)
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		g.decide(http.StatusOK)
	}
	if g.passing {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func (g *gzipWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) close() {
	if g.gz != nil {
		_ = g.gz.Close()
		gzPool.Put(g.gz)
		g.gz = nil
	}
}

// gzipMiddleware compresses responses for clients that ask for it. Content-Type
// decides, so fonts and images pass through untouched.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}
