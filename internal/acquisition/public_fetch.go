// Package acquisition provides the read-only network boundary for generated code.
package acquisition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/encoding/simplifiedchinese"
)

var ErrBudgetExceeded = errors.New("acquisition budget exceeded")
var ErrNotAllowed = errors.New("acquisition permission denied")

// DefaultMaxResponseBodyBytes bounds single HTTP acquisitions so oversized
// bulk catalog dumps fail fast before exhausting memory or DB packet limits.
const DefaultMaxResponseBodyBytes int64 = 16 * 1024 * 1024

type Policy struct {
	AllowedDomains []string      `json:"allowed_domains,omitempty"` // empty: all public HTTP(S), never private networks.
	Timeout        time.Duration `json:"-"`
	MaxBodyBytes   int64         `json:"-"`
}

// ConfiguredMaxResponseBytes returns the HTTP response byte ceiling configured
// via GORELAY_MAX_RESPONSE_BYTES, defaulting to DefaultMaxResponseBodyBytes.
func ConfiguredMaxResponseBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("GORELAY_MAX_RESPONSE_BYTES"))
	if raw == "" {
		return DefaultMaxResponseBodyBytes
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return DefaultMaxResponseBodyBytes
	}
	return v
}

// InspectionPolicy preserves the caller's source scope. Duration comes from context.
func InspectionPolicy(domains []string) Policy {
	return Policy{AllowedDomains: domains}
}

type Receipt struct {
	Method         string            `json:"method"`
	RequestedURL   string            `json:"requested_url,omitempty"`
	URL            string            `json:"url"`
	Status         int               `json:"status"`
	Bytes          int64             `json:"bytes"`
	FetchedAt      time.Time         `json:"fetched_at"`
	DurationMS     int64             `json:"duration_ms"`
	Headers        map[string]string `json:"headers,omitempty"`
	Cookies        map[string]string `json:"cookies,omitempty"`
	SetCookieNames []string          `json:"set_cookie_names,omitempty"`
	RequestProfile string            `json:"request_profile,omitempty"`
	SnapshotKey    string            `json:"snapshot_key,omitempty"`
	ReusedSnapshot bool              `json:"reused_snapshot,omitempty"`
}

// DiagnosticHeaders exposes HTTP metadata needed to choose the next action.
// Stateful response headers remain available inside the acquisition runtime.
func (r Receipt) DiagnosticHeaders() map[string]string {
	headers := make(map[string]string)
	for name, value := range r.Headers {
		switch strings.ToLower(name) {
		case "content-type", "content-length", "etag", "last-modified", "retry-after",
			"ratelimit", "ratelimit-limit", "ratelimit-remaining", "ratelimit-reset",
			"x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-reset":
			headers[strings.ToLower(name)] = value
		}
	}
	return headers
}

func (r Receipt) MarshalJSON() ([]byte, error) {
	type receiptJSON Receipt
	r.Headers = r.DiagnosticHeaders()
	r.SetCookieNames = r.DiagnosticCookieNames()
	r.Cookies = nil
	return json.Marshal(receiptJSON(r))
}

func (r Receipt) DiagnosticCookieNames() []string {
	if len(r.Cookies) == 0 {
		return r.SetCookieNames
	}
	names := make([]string, 0, len(r.Cookies))
	for name := range r.Cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NetworkAPICapture describes a compact browser XHR/Fetch JSON exchange for API discovery.
type NetworkAPICapture struct {
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	Status         int               `json:"status"`
	ContentType    string            `json:"content_type,omitempty"`
	RequestHeaders map[string]string `json:"request_headers,omitempty"`
	SentCookies    bool              `json:"sent_cookies,omitempty"`
	SetsCookie     bool              `json:"sets_cookie,omitempty"`
	RequestBody    string            `json:"request_body,omitempty"`
	ResponseSchema map[string]string `json:"response_json_paths,omitempty"`
}
type Response struct {
	Body    string  `json:"body"`
	Receipt Receipt `json:"receipt"`
}
type Stats struct {
	Requests int   `json:"requests"`
	Bytes    int64 `json:"bytes"`
}
type RequestOptions struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    string
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
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	f.transport = &http.Transport{DialContext: f.publicDial, TLSHandshakeTimeout: policy.Timeout, ResponseHeaderTimeout: policy.Timeout}
	f.client = &http.Client{Transport: budgetTransport{f}, Jar: jar, Timeout: policy.Timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		for _, prior := range via {
			if prior.URL.String() == req.URL.String() {
				return fmt.Errorf("source redirect cycle revisited %s", req.URL)
			}
		}
		return f.checkURL(req.URL)
	}}
	return f, nil
}
func (f *PublicFetcher) Close()       { f.transport.CloseIdleConnections() }
func (f *PublicFetcher) Stats() Stats { f.mu.Lock(); defer f.mu.Unlock(); return f.stats }

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
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

func checkPublicURL(u *url.URL) error {
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
	return nil
}

// DefaultAcceptLanguageForURL selects an appropriate default Accept-Language
// header from the target URL so global multi-locale sites do not auto-redirect
// English endpoints to regional subsidiaries unless the URL specifies one.
func DefaultAcceptLanguageForURL(u *url.URL) string {
	if u != nil {
		host := strings.ToLower(u.Hostname())
		path := strings.ToLower(u.Path)
		if strings.HasSuffix(host, ".cn") || strings.HasSuffix(host, ".hk") || strings.HasSuffix(host, ".tw") ||
			strings.HasPrefix(path, "/cn/") || path == "/cn" ||
			strings.HasPrefix(path, "/zh") || strings.Contains(path, "/zh-cn/") ||
			strings.Contains(path, "/zh-hans/") || strings.Contains(path, "/zh-hant/") {
			return "zh-CN,zh;q=0.9,en;q=0.8"
		}
	}
	return "en-US,en;q=0.9,zh-CN;q=0.8"
}

func isHopByHopResponseHeader(lower string) bool {
	switch lower {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func extractResponseHeadersAndCookies(resp *http.Response) (map[string]string, map[string]string) {
	if resp == nil {
		return nil, nil
	}
	hdrs := map[string]string{}
	for key, values := range resp.Header {
		if len(values) == 0 {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "" || isHopByHopResponseHeader(lower) {
			continue
		}
		sep := ", "
		if lower == "set-cookie" {
			sep = "\n"
		}
		joined := strings.TrimSpace(strings.Join(values, sep))
		if joined != "" {
			hdrs[lower] = joined
		}
	}
	var cookies map[string]string
	for _, c := range resp.Cookies() {
		if c != nil && c.Name != "" {
			if cookies == nil {
				cookies = map[string]string{}
			}
			cookies[c.Name] = c.Value
		}
	}
	return hdrs, cookies
}

func (f *PublicFetcher) checkURL(u *url.URL) error {
	if err := checkPublicURL(u); err != nil {
		return err
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
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
	ips, err := f.resolveIPs(ctx, host)
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
	return t.f.roundTrip(req)
}

func (f *PublicFetcher) roundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.stats.Requests++
	f.mu.Unlock()
	resp, err := f.transport.RoundTrip(req)
	if err == nil {
		resp.Body = &budgetBody{ReadCloser: resp.Body, f: f}
	}
	return resp, err
}

type budgetBody struct {
	io.ReadCloser
	f *PublicFetcher
}

func (b *budgetBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.f.mu.Lock()
	b.f.stats.Bytes += int64(n)
	b.f.mu.Unlock()
	return n, err
}

func (f *PublicFetcher) Fetch(ctx context.Context, method, rawURL string) (Response, error) {
	return f.fetchOnce(ctx, method, rawURL)
}

// FetchRequest executes GET/HEAD/POST with caller-supplied headers and body,
// returning the decoded body and response headers even on 4xx/5xx status codes
// so scripts and fault-recovery logic can diagnose anti-bot or API errors.
func (f *PublicFetcher) FetchRequest(ctx context.Context, opts RequestOptions) (Response, error) {
	return f.fetchWithOptions(ctx, opts, false)
}

// FetchWithSnapshots reuses a successful response within the caller's task.
// The identity is the effective GET. A direct, public representation that does
// not vary by User-Agent may also be reused under another User-Agent.
func (f *PublicFetcher) FetchWithSnapshots(ctx context.Context, opts RequestOptions, snapshots map[string]Response) (Response, error) {
	if len(snapshots) > 0 {
		req, err := f.sourceRequest(ctx, opts, false)
		if err != nil {
			return Response{}, err
		}
		for _, key := range f.snapshotCandidates(req) {
			saved, ok := snapshots[key]
			if !ok {
				continue
			}
			target, err := url.Parse(saved.Receipt.URL)
			if err == nil && f.checkURL(target) == nil {
				saved.Receipt.ReusedSnapshot = true
				saved.Receipt.DurationMS = 0
				return saved, nil
			}
		}
	}
	return f.FetchRequest(ctx, opts)
}

func (f *PublicFetcher) snapshotKey(req *http.Request) string {
	if req.Method != http.MethodGet || req.Body != nil || req.Header.Get("Authorization") != "" ||
		req.Header.Get("Cookie") != "" || req.Header.Get("Range") != "" || len(f.client.Jar.Cookies(req.URL)) > 0 {
		return ""
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "if-") {
			return ""
		}
	}
	return snapshotIdentity(req, false)
}

// SnapshotCandidates supplies the exact cache keys a host needs to load for a known request.
func (f *PublicFetcher) SnapshotCandidates(ctx context.Context, opts RequestOptions) []string {
	req, err := f.sourceRequest(ctx, opts, false)
	if err != nil {
		return nil
	}
	return f.snapshotCandidates(req)
}

func (f *PublicFetcher) snapshotCandidates(req *http.Request) []string {
	if exact := f.snapshotKey(req); exact != "" {
		return []string{exact, snapshotIdentity(req, true)}
	}
	return nil
}

func snapshotIdentity(req *http.Request, withoutUserAgent bool) string {
	u := *req.URL
	u.Fragment, u.RawFragment = "", ""
	headers := req.Header
	if withoutUserAgent {
		headers = headers.Clone()
		headers.Del("User-Agent")
	}
	key, _ := json.Marshal([]any{u.String(), headers})
	return fmt.Sprintf("%x", sha256.Sum256(key))
}

func allowsUserAgentSnapshotReuse(headers map[string]string) bool {
	public := false
	for _, directive := range strings.Split(strings.ToLower(headers["cache-control"]), ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(directive), "=")
		switch strings.TrimSpace(name) {
		case "public":
			public = true
		case "no-store", "no-cache", "private":
			return false
		}
	}
	for _, name := range strings.Split(strings.ToLower(headers["vary"]), ",") {
		switch strings.TrimSpace(name) {
		case "*", "user-agent":
			return false
		}
	}
	return public
}

func (f *PublicFetcher) fetchOnce(ctx context.Context, method, rawURL string) (Response, error) {
	return f.fetchWithOptions(ctx, RequestOptions{Method: method, URL: rawURL}, true)
}

const defaultBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

func decodeResponseUTF8(body []byte, contentType string) string {
	encoding, name, certain := charset.DetermineEncoding(body, contentType)
	if !utf8.Valid(body) || certain || (strings.Contains(strings.ToLower(contentType), "html") && name != "utf-8") {
		reader, decodeErr := charset.NewReader(bytes.NewReader(body), contentType)
		if decodeErr == nil {
			decoded, readErr := io.ReadAll(reader)
			if readErr == nil && len(decoded) > 0 {
				body = decoded
			}
		} else if encoding != nil {
			decoded, decErr := encoding.NewDecoder().Bytes(body)
			if decErr == nil && len(decoded) > 0 {
				body = decoded
			}
		}
	}
	if !utf8.Valid(body) {
		if decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(body); err == nil && utf8.Valid(decoded) {
			body = decoded
		} else {
			body = []byte(strings.ToValidUTF8(string(body), ""))
		}
	}
	return string(body)
}

func (f *PublicFetcher) sourceRequest(ctx context.Context, opts RequestOptions, strictStatus bool) (*http.Request, error) {
	method := strings.ToUpper(strings.TrimSpace(opts.Method))
	if method == "" {
		method = http.MethodGet
	}
	u, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid URL", ErrNotAllowed)
	}
	if err = f.checkURL(u); err != nil {
		return nil, err
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodPost {
		return nil, fmt.Errorf("%w: GET/HEAD/POST only", ErrNotAllowed)
	}
	if strictStatus && method == http.MethodPost {
		return nil, fmt.Errorf("%w: GET/HEAD only", ErrNotAllowed)
	}
	var bodyReader io.Reader
	if opts.Body != "" {
		bodyReader = strings.NewReader(opts.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultBrowserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/json;q=0.8,*/*;q=0.7")
	req.Header.Set("Accept-Language", DefaultAcceptLanguageForURL(u))
	for k, v := range opts.Headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

func (f *PublicFetcher) fetchWithOptions(ctx context.Context, opts RequestOptions, strictStatus bool) (Response, error) {
	start := time.Now()
	out := Response{}
	req, err := f.sourceRequest(ctx, opts, strictStatus)
	if err != nil {
		return out, err
	}
	snapshotKey := f.snapshotKey(req)
	resp, err := f.client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	hdrs, cookies := extractResponseHeadersAndCookies(resp)
	maxBody := f.policy.MaxBodyBytes
	var body []byte
	if maxBody > 0 && resp.ContentLength > maxBody {
		err = fmt.Errorf("response body (%d bytes) exceeds %d byte limit; inspect page links/endpoints or query a narrower/filtered endpoint", resp.ContentLength, maxBody)
	} else if maxBody > 0 {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err == nil && int64(len(body)) > maxBody {
			err = fmt.Errorf("response body exceeds %d byte limit; inspect page links/endpoints or query a narrower/filtered endpoint", maxBody)
			body = body[:maxBody]
		}
	} else {
		body, err = io.ReadAll(resp.Body)
	}
	byteCount := int64(len(body))
	if resp.ContentLength > byteCount {
		byteCount = resp.ContentLength
	}
	out.Receipt = Receipt{Method: req.Method, RequestedURL: opts.URL, URL: resp.Request.URL.String(), Status: resp.StatusCode, Bytes: byteCount, FetchedAt: time.Now().UTC(), DurationMS: time.Since(start).Milliseconds(), Headers: hdrs, Cookies: cookies}
	// Redirects or Set-Cookie can carry state that is not part of a saved document.
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Request.URL.String() == req.URL.String() && len(cookies) == 0 {
		out.Receipt.SnapshotKey = snapshotKey
		if snapshotKey != "" && allowsUserAgentSnapshotReuse(hdrs) {
			out.Receipt.SnapshotKey = snapshotIdentity(req, true)
		}
	}
	requestBody := ""
	if resp.Request.ContentLength > 0 {
		requestBody = opts.Body
	}
	profile, _ := json.Marshal([]any{resp.Request.Method, resp.Request.Header, requestBody})
	out.Receipt.RequestProfile = fmt.Sprintf("%x", sha256.Sum256(profile))
	if err != nil {
		return out, err
	}
	if strictStatus && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return out, fmt.Errorf("source HTTP status %d", resp.StatusCode)
	}
	out.Body = decodeResponseUTF8(body, resp.Header.Get("Content-Type"))
	return out, nil
}
