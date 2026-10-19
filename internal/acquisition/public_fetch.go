// Package acquisition provides the read-only network boundary for generated code.
package acquisition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

var ErrBudgetExceeded = errors.New("acquisition budget exceeded")
var ErrNotAllowed = errors.New("acquisition permission denied")

type Policy struct {
	AllowedDomains []string      `json:"allowed_domains,omitempty"` // empty: all public HTTP(S), never private networks.
	MaxRequests    int           `json:"max_requests"`
	MaxBytes       int64         `json:"max_bytes"`
	Timeout        time.Duration `json:"-"`
	MaxAttempts    int           `json:"-"` // Host configured, never generated code.
}

// InspectionPolicy bounds acquisition independently of the model's receipt view.
func InspectionPolicy(domains []string) Policy {
	return Policy{AllowedDomains: domains, MaxRequests: 4, MaxBytes: 1 << 20, Timeout: 15 * time.Second}
}

type Receipt struct {
	Method       string    `json:"method"`
	RequestedURL string    `json:"requested_url,omitempty"`
	URL          string    `json:"url"`
	Status       int       `json:"status"`
	Bytes        int64     `json:"bytes"`
	FetchedAt    time.Time `json:"fetched_at"`
	DurationMS   int64     `json:"duration_ms"`
}
type Response struct {
	Body    string  `json:"body"`
	Receipt Receipt `json:"receipt"`
}
type Stats struct {
	Requests int   `json:"requests"`
	Bytes    int64 `json:"bytes"`
}
type FetchFunc func(context.Context, string, string) (Response, error)

type PublicFetcher struct {
	policy    Policy
	client    *http.Client
	transport *http.Transport
	lookupIP  func(context.Context, string, string) ([]net.IP, error)
	dial      func(context.Context, string, string) (net.Conn, error)
	proxyURL  *url.URL
	mu        sync.Mutex
	stats     Stats
}

func NewPublicFetcher(policy Policy) (*PublicFetcher, error) {
	if policy.MaxRequests <= 0 || policy.MaxBytes <= 0 {
		return nil, fmt.Errorf("positive request and byte budgets are required")
	}
	if policy.Timeout <= 0 {
		policy.Timeout = 20 * time.Second
	}
	if policy.MaxAttempts == 0 {
		policy.MaxAttempts = 2
	}
	if policy.MaxAttempts < 1 || policy.MaxAttempts > 3 {
		return nil, fmt.Errorf("acquisition attempts must be between 1 and 3")
	}
	policy.AllowedDomains = append([]string{}, policy.AllowedDomains...)
	for i, domain := range policy.AllowedDomains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		domain = strings.TrimSuffix(domain, ".")
		if domain == "" || strings.ContainsAny(domain, "/:@* \\?#") {
			return nil, fmt.Errorf("scope must contain plain domain names")
		}
		policy.AllowedDomains[i] = domain
	}
	d := &net.Dialer{Timeout: policy.Timeout}
	f := &PublicFetcher{policy: policy, lookupIP: net.DefaultResolver.LookupIP, dial: d.DialContext}
	if err := f.configureProxy(); err != nil {
		return nil, err
	}
	f.transport = &http.Transport{Proxy: nil, DialContext: f.publicDial, TLSHandshakeTimeout: policy.Timeout, ResponseHeaderTimeout: policy.Timeout, MaxIdleConns: 4, MaxConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	f.client = &http.Client{Transport: budgetTransport{f}, Timeout: policy.Timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("redirect limit exceeded")
		}
		return f.checkURL(req.URL)
	}}
	return f, nil
}
func (f *PublicFetcher) Close()       { f.transport.CloseIdleConnections() }
func (f *PublicFetcher) Stats() Stats { f.mu.Lock(); defer f.mu.Unlock(); return f.stats }

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"),
}

func isPublicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func (f *PublicFetcher) checkURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("%w: public HTTP(S) URL required", ErrNotAllowed)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("%w: non-public host", ErrNotAllowed)
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return fmt.Errorf("%w: non-public IP", ErrNotAllowed)
	}
	if len(f.policy.AllowedDomains) > 0 {
		allowed := false
		for _, domain := range f.policy.AllowedDomains {
			if host == domain || strings.HasSuffix(host, "."+domain) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%w: host outside granted source scope", ErrNotAllowed)
		}
	}
	return nil
}

// Resolve once, verify every address, and dial the verified IP rather than let
// a second DNS lookup change the peer. TLS still verifies the original hostname.
func (f *PublicFetcher) publicDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := f.lookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS returned no addresses")
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("%w: DNS resolved a non-public address", ErrNotAllowed)
		}
	}
	var last error
	for _, ip := range ips {
		conn, e := f.dialPublicPeer(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return conn, nil
		}
		last = e
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

type budgetTransport struct{ f *PublicFetcher }

func (t budgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.f.checkURL(req.URL); err != nil {
		return nil, err
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, fmt.Errorf("%w: GET/HEAD only", ErrNotAllowed)
	}
	t.f.mu.Lock()
	if t.f.stats.Requests >= t.f.policy.MaxRequests || t.f.stats.Bytes >= t.f.policy.MaxBytes {
		t.f.mu.Unlock()
		return nil, ErrBudgetExceeded
	}
	t.f.stats.Requests++ // Errors and redirect hops also spend the same operation budget.
	t.f.mu.Unlock()
	resp, err := t.f.transport.RoundTrip(req)
	if err == nil {
		resp.Body = &budgetBody{ReadCloser: resp.Body, f: t.f}
	}
	return resp, err
}

type budgetBody struct {
	io.ReadCloser
	f *PublicFetcher
}

func (b *budgetBody) Read(p []byte) (int, error) {
	b.f.mu.Lock()
	left := b.f.policy.MaxBytes - b.f.stats.Bytes
	b.f.mu.Unlock()
	if left < 0 {
		return 0, ErrBudgetExceeded
	}
	if int64(len(p)) > left+1 {
		p = p[:int(left+1)]
	}
	n, err := b.ReadCloser.Read(p)
	b.f.mu.Lock()
	b.f.stats.Bytes += int64(n)
	exceeded := b.f.stats.Bytes > b.f.policy.MaxBytes
	b.f.mu.Unlock()
	if exceeded {
		return n, ErrBudgetExceeded
	}
	return n, err
}

func (f *PublicFetcher) Fetch(ctx context.Context, method, rawURL string) (Response, error) {
	// The complete operation has a finite ceiling, even when a caller supplies
	// no deadline. Each physical attempt/redirect still shares request/byte caps.
	ctx, cancel := context.WithTimeout(ctx, f.policy.Timeout*time.Duration(f.policy.MaxAttempts))
	defer cancel()
	var out Response
	var err error
	for attempt := 0; attempt < f.policy.MaxAttempts; attempt++ {
		out, err = f.fetchOnce(ctx, method, rawURL)
		if err == nil || !retryableFetch(ctx, out, err) || attempt+1 == f.policy.MaxAttempts {
			return out, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
	return out, err
}

func retryableFetch(ctx context.Context, out Response, err error) bool {
	if ctx.Err() != nil || errors.Is(err, ErrNotAllowed) || errors.Is(err, ErrBudgetExceeded) {
		return false
	}
	if out.Receipt.Status == 502 || out.Receipt.Status == 503 || out.Receipt.Status == 504 {
		return true
	}
	var temporary net.Error
	return errors.As(err, &temporary) && (temporary.Timeout() || temporary.Temporary())
}

func (f *PublicFetcher) fetchOnce(ctx context.Context, method, rawURL string) (Response, error) {
	start := time.Now()
	out := Response{}
	u, err := url.Parse(rawURL)
	if err != nil {
		return out, fmt.Errorf("%w: invalid URL", ErrNotAllowed)
	}
	if err = f.checkURL(u); err != nil {
		return out, err
	}
	if method != http.MethodGet && method != http.MethodHead {
		return out, fmt.Errorf("%w: GET/HEAD only", ErrNotAllowed)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("User-Agent", "GoRelay/1.0 public-readonly-collector")
	req.Header.Set("Accept", "application/json,text/html,text/plain;q=0.9,*/*;q=0.5")
	resp, err := f.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	out.Receipt = Receipt{Method: method, RequestedURL: rawURL, URL: resp.Request.URL.String(), Status: resp.StatusCode, Bytes: int64(len(body)), FetchedAt: time.Now().UTC(), DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		return out, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("source HTTP status %d", resp.StatusCode)
	}
	// Decode once at the acquisition boundary. Byte accounting remains the
	// received wire bytes; model views, Lua and evidence share the UTF-8 text.
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(contentType), "html") {
		_, _, declared := charset.DetermineEncoding(body, contentType)
		if declared || !utf8.Valid(body) {
			reader, decodeErr := charset.NewReader(bytes.NewReader(body), contentType)
			if decodeErr != nil {
				return out, decodeErr
			}
			decoded, decodeErr := io.ReadAll(io.LimitReader(reader, f.policy.MaxBytes*4+1))
			if decodeErr != nil {
				return out, decodeErr
			}
			if int64(len(decoded)) > f.policy.MaxBytes*4 {
				return out, fmt.Errorf("decoded text exceeds host representation limit")
			}
			body = decoded
		}
	}
	if !utf8.Valid(body) {
		return out, fmt.Errorf("source response is not valid UTF-8; declare a supported HTML charset")
	}
	out.Body = string(body)
	return out, nil
}
