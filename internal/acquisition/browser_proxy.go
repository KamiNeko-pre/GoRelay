package acquisition

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// BrowserProxy reuses the existing public-DNS pinning, domain scope, host
// proxy and byte budget. It preserves the browser's real request headers and
// cookies. The trusted browser runner separately enforces GET/HEAD per request
// inside encrypted tunnels; no arbitrary browser/CDP endpoint is exposed.
func (f *PublicFetcher) BrowserProxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			u, err := url.Parse("https://" + r.Host + "/")
			if err != nil || f.checkURL(u) != nil {
				http.Error(w, "destination outside public browser scope", http.StatusForbidden)
				return
			}
			f.mu.Lock()
			allowed := f.stats.Requests < f.policy.MaxRequests && f.stats.Bytes < f.policy.MaxBytes
			if allowed {
				f.stats.Requests++
			}
			f.mu.Unlock()
			if !allowed {
				http.Error(w, ErrBudgetExceeded.Error(), 429)
				return
			}
			conn, err := f.publicDial(r.Context(), "tcp", r.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			client, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				conn.Close()
				return
			}
			defer client.Close()
			defer conn.Close()
			fmt.Fprint(buffer, "HTTP/1.1 200 Connection Established\r\n\r\n")
			buffer.Flush()
			done := make(chan struct{})
			go func() { io.Copy(conn, buffer); conn.Close(); close(done) }()
			io.Copy(client, &budgetBody{ReadCloser: conn, f: f})
			client.Close()
			<-done
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "browser acquisition permits GET/HEAD only", 403)
			return
		}
		if err := f.checkURL(r.URL); err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		req := r.Clone(r.Context())
		req.RequestURI = ""
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Proxy-Connection")
		resp, err := (budgetTransport{f: f}).RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for name, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})
}
