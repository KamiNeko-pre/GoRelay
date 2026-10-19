package acquisition

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyTunnelsOnlyPinnedPublicIPsAndPreservesTLSHostname(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP", true: "HTTPS"}[secure], func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("proxy credentials leaked to source")
				}
				w.Write([]byte("verified"))
			})
			var source *httptest.Server
			if secure {
				source = httptest.NewTLSServer(handler)
			} else {
				source = httptest.NewServer(handler)
			}
			defer source.Close()
			connects := 0
			port := "80"
			scheme := "http"
			if secure {
				port = "443"
				scheme = "https"
			}
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connects++
				if r.Method != "CONNECT" || r.Host != "8.8.8.8:"+port {
					t.Errorf("proxy received unpinned destination %s %s", r.Method, r.Host)
					w.WriteHeader(400)
					return
				}
				upstream, err := net.Dial("tcp", strings.TrimPrefix(strings.TrimPrefix(source.URL, "http://"), "https://"))
				if err != nil {
					w.WriteHeader(502)
					return
				}
				defer upstream.Close()
				conn, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				buffer.Flush()
				go io.Copy(upstream, buffer)
				io.Copy(conn, upstream)
			}))
			defer proxy.Close()
			t.Setenv("GORELAY_ACQUISITION_PROXY", proxy.URL)
			f, err := NewPublicFetcher(InspectionPolicy(nil))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			f.lookupIP = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
			if secure {
				pool := x509.NewCertPool()
				pool.AddCert(source.Certificate())
				f.transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			}
			got, err := f.Fetch(context.Background(), "GET", scheme+"://example.com/data")
			if err != nil || got.Body != "verified" {
				t.Fatalf("verified source unavailable: %v", err)
			}
			before := connects
			f.transport.CloseIdleConnections()
			// Mixed public/private DNS must be rejected before opening any tunnel.
			f.lookupIP = func(context.Context, string, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.1")}, nil
			}
			_, err = f.Fetch(context.Background(), "GET", scheme+"://another.example/data")
			if !errors.Is(err, ErrNotAllowed) || connects != before {
				t.Fatalf("proxy bypassed public DNS validation: connects=%d err=%v", connects, err)
			}
		})
	}
}
