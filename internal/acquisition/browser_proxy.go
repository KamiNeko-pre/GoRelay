package acquisition

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// BrowserProxy reuses the existing public-DNS pinning, domain scope, host
// proxy. The trusted runner restricts document navigation to the caller's source
// scope while allowing the public resources needed to render it. No arbitrary
// browser/CDP endpoint or caller credentials are exposed.
func (f *PublicFetcher) BrowserProxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			u, err := url.Parse("https://" + r.Host + "/")
			if err != nil || checkPublicURL(u) != nil {
				http.Error(w, "destination outside public browser scope", http.StatusForbidden)
				return
			}
			f.mu.Lock()
			f.stats.Requests++
			f.mu.Unlock()
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
		if err := checkPublicURL(r.URL); err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		req := r.Clone(r.Context())
		req.RequestURI = ""
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Proxy-Connection")
		resp, err := f.roundTrip(req)
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
