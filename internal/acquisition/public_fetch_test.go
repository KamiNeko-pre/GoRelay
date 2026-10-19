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

func TestPublicFetcherPinsCheckedIPAndBlocksRebindingRedirect(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://internal.test/secret", 302)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer s.Close()
	f, err := NewPublicFetcher(Policy{MaxRequests: 3, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.lookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
		if host == "internal.test" {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	d := net.Dialer{}
	f.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "8.8.8.8:80" {
			t.Errorf("not pinned: %s", addr)
		}
		return d.DialContext(ctx, network, strings.TrimPrefix(s.URL, "http://"))
	}
	got, err := f.Fetch(context.Background(), http.MethodGet, "http://public.test/data")
	if err != nil || got.Body != `{"ok":true}` || got.Receipt.URL != "http://public.test/data" {
		t.Fatalf("bad fetch: %+v %v", got, err)
	}
	_, err = f.Fetch(context.Background(), http.MethodGet, "http://public.test/redirect")
	if err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("private DNS redirect accepted: %v", err)
	}
}

func TestFetcherScopeAndStickyBudget(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("12345")) }))
	defer s.Close()
	f, _ := NewPublicFetcher(Policy{AllowedDomains: []string{"example.com"}, MaxRequests: 1, MaxBytes: 4})
	defer f.Close()
	if _, err := f.Fetch(context.Background(), http.MethodGet, "https://notexample.com/"); err == nil {
		t.Fatal("suffix domain bypass")
	}
	if _, err := f.Fetch(context.Background(), http.MethodPost, "https://example.com/"); err == nil {
		t.Fatal("POST accepted")
	}
	f.lookupIP = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	d := net.Dialer{}
	f.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, strings.TrimPrefix(s.URL, "http://"))
	}
	_, err := f.Fetch(context.Background(), http.MethodGet, "https://example.com/")
	// HTTPS deliberately cannot talk to this HTTP fixture: it still spends the attempt.
	if err == nil {
		t.Fatal("fixture expected TLS failure")
	}
	_, err = f.Fetch(context.Background(), http.MethodGet, "http://example.com/")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("attempt budget reset after error: %v", err)
	}
	g, _ := NewPublicFetcher(Policy{MaxRequests: 2, MaxBytes: 4})
	defer g.Close()
	g.lookupIP = f.lookupIP
	g.dial = f.dial
	_, err = g.Fetch(context.Background(), http.MethodGet, "http://example.com/")
	if !errors.Is(err, ErrBudgetExceeded) || g.Stats().Bytes != 5 {
		t.Fatalf("body budget not enforced: %+v %v", g.Stats(), err)
	}
}

func TestPublicIPPolicy(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "0.0.0.0", "::1", "::ffff:127.0.0.1", "fc00::1", "100.100.100.200", "192.0.2.1"} {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("accepted non-public %s", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(net.ParseIP(s)) {
			t.Errorf("rejected public %s", s)
		}
	}
}
