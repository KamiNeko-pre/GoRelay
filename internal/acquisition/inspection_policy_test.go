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

func TestInspectionReadsOrdinaryDocumentsAndKeepsHardBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		exceeded   bool
	}{
		{"large-html", "<html><body>" + strings.Repeat("x", 400<<10) + "</body></html>", 200, false},
		{"large-json", `{"description":"` + strings.Repeat("y", 600<<10) + `"}`, 200, false},
		{"small-existing", `{"version":"1.0"}`, 200, false},
		{"oversized", strings.Repeat("z", (1<<20)+1), 200, true},
		{"permission-denied", "denied", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer server.Close()
			f, err := NewPublicFetcher(InspectionPolicy([]string{"public.test"}))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			f.lookupIP = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
			d := net.Dialer{}
			f.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, strings.TrimPrefix(server.URL, "http://"))
			}
			out, err := f.Fetch(context.Background(), http.MethodGet, "http://public.test/document")
			if tc.exceeded {
				if !errors.Is(err, ErrBudgetExceeded) || out.Body != "" {
					t.Fatalf("oversized source accepted: %v", err)
				}
				return
			}
			if tc.status == 403 {
				if err == nil || out.Body != "" {
					t.Fatal("denied source accepted")
				}
				return
			}
			if err != nil || out.Body != tc.body || out.Receipt.Bytes != int64(len(tc.body)) || f.Stats().Bytes != int64(len(tc.body)) {
				t.Fatalf("ordinary document lost: bytes=%d error=%v", f.Stats().Bytes, err)
			}
		})
	}
}
