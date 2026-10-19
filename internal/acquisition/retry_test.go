package acquisition

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicFetcherRetriesOnlyTransientFailures(t *testing.T) {
	for _, mode := range []string{"503", "timeout", "403", "persistent503", "private_redirect"} {
		t.Run(mode, func(t *testing.T) {
			requests, dials := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if mode == "private_redirect" {
					http.Redirect(w, r, "http://127.0.0.1/secret", 302)
					return
				}
				if mode == "403" {
					w.WriteHeader(403)
					return
				}
				if mode == "persistent503" || mode == "503" && requests == 1 {
					w.WriteHeader(503)
					return
				}
				w.Write([]byte("ready"))
			}))
			defer server.Close()
			f, err := NewPublicFetcher(InspectionPolicy(nil))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			f.lookupIP = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
			f.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				dials++
				if mode == "timeout" && dials == 1 {
					return nil, &net.DNSError{Err: "fixture timeout", IsTimeout: true}
				}
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "http://"))
			}
			got, err := f.Fetch(context.Background(), "GET", "http://public.test/data")
			if mode == "503" || mode == "timeout" {
				if err != nil || got.Body != "ready" || f.Stats().Requests != 2 {
					t.Fatalf("transient failure not recovered: %+v stats=%+v err=%v", got, f.Stats(), err)
				}
			} else {
				if err == nil {
					t.Fatal("permanent failure became success")
				}
				want := 1
				if mode == "persistent503" {
					want = 2
				}
				if requests != want {
					t.Fatalf("incorrect retry count: %d, want %d", requests, want)
				}
				if mode == "private_redirect" && !errors.Is(err, ErrNotAllowed) {
					t.Fatalf("lost SSRF rejection: %v", err)
				}
			}
		})
	}
}
