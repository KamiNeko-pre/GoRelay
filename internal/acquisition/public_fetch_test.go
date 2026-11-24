package acquisition

import (
	"bytes"
	"context"
	"encoding/json"

	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestReceiptSerializationKeepsHTTPDiagnosticsWithoutSessionValues(t *testing.T) {
	for _, header := range []string{"set-cookie", "x-csrf-token"} {
		t.Run(header, func(t *testing.T) {
			r := Receipt{Headers: map[string]string{header: "private-value", "retry-after": "30"}, Cookies: map[string]string{"session": "private-value"}}
			encoded, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private-value") || !strings.Contains(string(encoded), `"retry-after":"30"`) {
				t.Fatalf("unexpected public HTTP metadata: %s", encoded)
			}
			if !strings.Contains(string(encoded), `"set_cookie_names":["session"]`) {
				t.Fatalf("missing cookie-name metadata: %s", encoded)
			}
			if r.Cookies["session"] != "private-value" || r.Headers[header] != "private-value" {
				t.Fatal("serializing a receipt changed the in-memory HTTP response")
			}
		})
	}
}

func TestPublicFetcherPinsCheckedIPAndBlocksRebindingRedirect(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://internal.test/secret", 302)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer s.Close()
	f, err := NewPublicFetcher(Policy{})
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

func TestFetcherEnforcesCallerScopeAndReadOnlyMethod(t *testing.T) {
	f, _ := NewPublicFetcher(Policy{AllowedDomains: []string{"example.com"}})
	defer f.Close()
	if _, err := f.Fetch(context.Background(), http.MethodGet, "https://notexample.com/"); err == nil {
		t.Fatal("suffix domain bypass")
	}
	if _, err := f.Fetch(context.Background(), http.MethodPost, "https://example.com/"); err == nil {
		t.Fatal("POST accepted")
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

func TestDefaultAcceptLanguageForURL(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://aws.amazon.com/s3/pricing/", "en-US,en;q=0.9,zh-CN;q=0.8"},
		{"https://www.backblaze.com/cloud-storage/pricing", "en-US,en;q=0.9,zh-CN;q=0.8"},
		{"https://aws.amazon.com/cn/s3/pricing/", "zh-CN,zh;q=0.9,en;q=0.8"},
		{"https://example.com.cn/jobs", "zh-CN,zh;q=0.9,en;q=0.8"},
		{"https://example.com/zh-cn/docs", "zh-CN,zh;q=0.9,en;q=0.8"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := DefaultAcceptLanguageForURL(u); got != tc.want {
			t.Errorf("DefaultAcceptLanguageForURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestFetchResponseForwardsHeadersAndCookies(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "csrf_tok", Value: "abc123", Path: "/"})
		w.Header().Set("ETag", `"v42"`)
		w.Header().Set("Last-Modified", "Wed, 18 Nov 2026 10:00:00 GMT")
		w.Header().Set("X-RateLimit-Remaining", "99")
		w.Header().Set("Connection", "close")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer s.Close()
	f, err := NewPublicFetcher(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.lookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8")}, nil
	}
	d := net.Dialer{}
	f.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, strings.TrimPrefix(s.URL, "http://"))
	}
	got, err := f.Fetch(context.Background(), http.MethodGet, "http://public.test/api")
	if err != nil {
		t.Fatal(err)
	}
	if got.Receipt.Cookies["csrf_tok"] != "abc123" {
		t.Fatalf("expected csrf_tok cookie parsed, got %+v", got.Receipt.Cookies)
	}
	if got.Receipt.Headers["etag"] != `"v42"` || got.Receipt.Headers["last-modified"] == "" || got.Receipt.Headers["x-ratelimit-remaining"] != "99" {
		t.Fatalf("expected metadata headers forwarded, got %+v", got.Receipt.Headers)
	}
	if _, hasHop := got.Receipt.Headers["connection"]; hasHop {
		t.Fatalf("expected hop-by-hop connection header excluded, got %+v", got.Receipt.Headers)
	}
}

func TestPublicFetcherSessionCookieJar(t *testing.T) {
	var seenCookie string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/csrf":
			http.SetCookie(w, &http.Cookie{Name: "session_csrf", Value: "tok_999", Path: "/"})
			w.Write([]byte(`{"ok":true}`))
		case "/search":
			if c, err := r.Cookie("session_csrf"); err == nil {
				seenCookie = c.Value
			}
			w.Write([]byte(`{"items":[1,2]}`))
		}
	}))
	defer s.Close()
	makeFetcher := func() *PublicFetcher {
		f, err := NewPublicFetcher(Policy{})
		if err != nil {
			t.Fatal(err)
		}
		f.lookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		}
		d := net.Dialer{}
		f.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, strings.TrimPrefix(s.URL, "http://"))
		}
		return f
	}
	f1 := makeFetcher()
	defer f1.Close()
	r1, err := f1.FetchRequest(context.Background(), RequestOptions{Method: http.MethodPost, URL: "http://public.test/csrf"})
	if err != nil || r1.Receipt.Cookies["session_csrf"] != "tok_999" {
		t.Fatalf("expected csrf cookie set, got %+v err=%v", r1.Receipt, err)
	}
	if _, err := f1.FetchRequest(context.Background(), RequestOptions{Method: http.MethodPost, URL: "http://public.test/search"}); err != nil {
		t.Fatal(err)
	}
	if seenCookie != "tok_999" {
		t.Fatalf("expected cookie jar to replay session_csrf=tok_999 on second request, got %q", seenCookie)
	}
	// Separate fetcher must have an isolated jar.
	seenCookie = ""
	f2 := makeFetcher()
	defer f2.Close()
	if _, err := f2.FetchRequest(context.Background(), RequestOptions{Method: http.MethodPost, URL: "http://public.test/search"}); err != nil {
		t.Fatal(err)
	}
	if seenCookie != "" {
		t.Fatalf("expected isolated cookie jar on new fetcher, got %q", seenCookie)
	}
}

func TestFetcherReusesEquivalentTaskSnapshot(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/shared", "/variant", "/session":
			w.Header().Set("Cache-Control", "public")
			if r.URL.Path == "/variant" {
				w.Header().Set("Vary", "Accept-Language, User-Agent")
			}
			if r.URL.Path == "/session" {
				http.SetCookie(w, &http.Cookie{Name: "session", Value: "active", Path: "/session"})
			}
		}
		w.Write([]byte(r.URL.RawQuery + r.Header.Get("Accept-Language") + r.Header.Get("Cookie")))
	}))
	defer s.Close()
	f, err := NewPublicFetcher(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.lookupIP = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	f.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(s.URL, "http://"))
	}
	ctx := context.Background()
	request := RequestOptions{URL: "http://public.test/prices?region=1", Headers: map[string]string{"Accept-Language": "en"}}
	first, err := f.FetchWithSnapshots(ctx, request, nil)
	if err != nil || first.Receipt.SnapshotKey == "" {
		t.Fatalf("unable to capture public GET: %+v, %v", first.Receipt, err)
	}
	snapshots := map[string]Response{first.Receipt.SnapshotKey: first}
	reused, err := f.FetchWithSnapshots(ctx, request, snapshots)
	if err != nil || !reused.Receipt.ReusedSnapshot || reused.Body != first.Body || reused.Receipt.FetchedAt != first.Receipt.FetchedAt || f.Stats().Requests != 1 {
		t.Fatalf("identical GET went to network or changed source identity: %+v, %v", reused, err)
	}
	for _, changed := range []RequestOptions{
		{URL: "http://public.test/prices?region=2", Headers: request.Headers},
		{URL: request.URL, Headers: map[string]string{"Accept-Language": "zh"}},
		{URL: request.URL, Headers: map[string]string{"Accept-Language": "en", "Cookie": "session=yes"}},
	} {
		got, err := f.FetchWithSnapshots(ctx, changed, snapshots)
		if err != nil || got.Receipt.ReusedSnapshot || got.Body == first.Body {
			t.Fatalf("different request incorrectly reused prior body: %+v, %v", got, err)
		}
	}
	for _, example := range []struct {
		path  string
		reuse bool
	}{{"/shared", true}, {"/variant", false}, {"/session", false}} {
		t.Run(example.path, func(t *testing.T) {
			request := RequestOptions{URL: "http://public.test" + example.path}
			first, err := f.FetchWithSnapshots(ctx, request, nil)
			if err != nil {
				t.Fatal(err)
			}
			snapshots := map[string]Response{first.Receipt.SnapshotKey: first}
			request.Headers = map[string]string{"User-Agent": "another public browser"}
			before := f.Stats().Requests
			got, err := f.FetchWithSnapshots(ctx, request, snapshots)
			if err != nil || got.Receipt.ReusedSnapshot != example.reuse || (f.Stats().Requests == before) != example.reuse {
				t.Fatalf("unexpected reuse under a different User-Agent: %+v, %v", got.Receipt, err)
			}
			if example.path == "/session" && !strings.Contains(got.Body, "session=active") {
				t.Fatalf("session cookie did not reach the next request: %q", got.Body)
			}
		})
	}
}

func TestPublicFetcherMaxBodyBytes(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chunked" {
			w.Write(bytes.Repeat([]byte("a"), 128))
			return
		}
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("b"), 1024))
	}))
	defer s.Close()
	f, err := NewPublicFetcher(Policy{MaxBodyBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.lookupIP = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	f.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(s.URL, "http://"))
	}
	if _, err := f.FetchRequest(context.Background(), RequestOptions{URL: "http://public.test/sized"}); err == nil || !strings.Contains(err.Error(), "exceeds 64 byte limit") {
		t.Fatalf("expected Content-Length rejection, got err=%v", err)
	}
	if _, err := f.FetchRequest(context.Background(), RequestOptions{URL: "http://public.test/chunked"}); err == nil || !strings.Contains(err.Error(), "exceeds 64 byte limit") {
		t.Fatalf("expected streaming body limit rejection, got err=%v", err)
	}
}
