package fetchgov

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gorelay/internal/acquisition"
)

type Config struct {
	Policy           acquisition.Policy
	MaxBodyBytes     int64
	RespectRobots    bool
	RobotsTTL        time.Duration
	RequestsPerSec   float64
	Burst            int
	CircuitThreshold int
	CircuitCooldown  time.Duration
	UserAgent        string
	AuthProfiles     map[string]AuthProfile
	Renderer         PageRenderer
	RequesterFactory func(policy acquisition.Policy) (Requester, error)
}

func DefaultConfig() Config {
	return Config{
		MaxBodyBytes:     DefaultMaxBodyBytes,
		RespectRobots:    true,
		RobotsTTL:        DefaultRobotsTTL,
		RequestsPerSec:   DefaultRequestsPerSec,
		Burst:            DefaultBurst,
		CircuitThreshold: DefaultCircuitThreshold,
		CircuitCooldown:  DefaultCircuitCooldown,
		UserAgent:        "gorelay",
	}
}

// LoadAuthProfilesFromJSON parses GORELAY_AUTH_PROFILES_JSON as either a JSON
// array of AuthProfile objects or a name-keyed JSON map.
func LoadAuthProfilesFromJSON(raw string) (map[string]AuthProfile, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	profiles := make(map[string]AuthProfile)
	if strings.HasPrefix(raw, "[") {
		var list []AuthProfile
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, fmt.Errorf("parse GORELAY_AUTH_PROFILES_JSON array: %w", err)
		}
		for _, p := range list {
			name := strings.TrimSpace(p.Name)
			if name == "" {
				continue
			}
			p.Name = name
			profiles[name] = p
		}
		return profiles, nil
	}
	var mapped map[string]AuthProfile
	if err := json.Unmarshal([]byte(raw), &mapped); err != nil {
		return nil, fmt.Errorf("parse GORELAY_AUTH_PROFILES_JSON object: %w", err)
	}
	for k, p := range mapped {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimSpace(k)
		}
		if name == "" {
			continue
		}
		p.Name = name
		profiles[name] = p
	}
	return profiles, nil
}

// ConfigFromEnv builds a SafeClient Config from standard GORELAY_* environment
// variables so server, worker, and lua-runner enforce identical governance.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()
	cfg.RespectRobots = !strings.EqualFold(strings.TrimSpace(os.Getenv("GORELAY_RESPECT_ROBOTS")), "false")
	if rawProfiles := strings.TrimSpace(os.Getenv("GORELAY_AUTH_PROFILES_JSON")); rawProfiles != "" {
		profiles, err := LoadAuthProfilesFromJSON(rawProfiles)
		if err != nil {
			return cfg, err
		}
		cfg.AuthProfiles = profiles
	}
	if sidecarURL := strings.TrimSpace(os.Getenv("GORELAY_BROWSER_CDP_URL")); sidecarURL != "" {
		renderer, err := NewHTTPPageRenderer(sidecarURL, os.Getenv("GORELAY_BROWSER_CDP_TOKEN"), 2)
		if err != nil {
			return cfg, fmt.Errorf("init browser sidecar renderer: %w", err)
		}
		cfg.Renderer = renderer
	}
	return cfg, nil
}

// SafeClient enforces shared SSRF, robots.txt, rate-limit, circuit-breaker,
// content-type, payload-size, and AuthProfile rules across all acquisition modes.
type SafeClient struct {
	cfg       Config
	robots    *RobotsCache
	governor  *DomainGovernor
	mu        sync.Mutex
	profiles  map[string]AuthProfile
	requester Requester
}

func NewSafeClient(cfg Config) (*SafeClient, error) {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "gorelay"
	}
	if cfg.RequesterFactory == nil {
		cfg.RequesterFactory = func(p acquisition.Policy) (Requester, error) {
			return acquisition.NewPublicFetcher(p)
		}
	}
	pol := cfg.Policy
	if pol.MaxBodyBytes <= 0 || pol.MaxBodyBytes > cfg.MaxBodyBytes {
		pol.MaxBodyBytes = cfg.MaxBodyBytes
	}
	if pol.Timeout <= 0 {
		pol.Timeout = 15 * time.Second
	}
	req, err := cfg.RequesterFactory(pol)
	if err != nil {
		return nil, err
	}
	profiles := make(map[string]AuthProfile, len(cfg.AuthProfiles))
	for k, v := range cfg.AuthProfiles {
		profiles[k] = v
	}
	return &SafeClient{
		cfg:       cfg,
		robots:    NewRobotsCache(cfg.RobotsTTL, cfg.UserAgent),
		governor:  NewDomainGovernor(cfg.RequestsPerSec, cfg.Burst, cfg.CircuitThreshold, cfg.CircuitCooldown),
		profiles:  profiles,
		requester: req,
	}, nil
}

func (c *SafeClient) Close() {
	if c.requester != nil {
		c.requester.Close()
	}
}

func (c *SafeClient) RegisterAuthProfile(p AuthProfile) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("auth profile name is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profiles == nil {
		c.profiles = make(map[string]AuthProfile)
	}
	c.profiles[p.Name] = p
	return nil
}

func (c *SafeClient) Fetch(ctx context.Context, req FetchRequest) (acquisition.Response, error) {
	parsed, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || parsed.Hostname() == "" {
		return acquisition.Response{}, &GovernanceError{Code: ErrCodeSSRFBlocked, Message: "invalid HTTP(S) target URL", Err: err}
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if err := checkAllowedDomains(host, req.AllowedDomains); err != nil {
		return acquisition.Response{}, err
	}

	opts := acquisition.RequestOptions{
		Method:  req.Method,
		URL:     parsed.String(),
		Headers: cloneStringMap(req.Headers),
		Body:    req.Body,
	}
	if opts.Method == "" {
		opts.Method = http.MethodGet
	}

	if req.AuthProfile != "" {
		if err := c.applyAuthProfile(req.AuthProfile, host, &opts); err != nil {
			return acquisition.Response{}, err
		}
	}

	var crawlDelay time.Duration
	if c.cfg.RespectRobots {
		delay, robotsErr := c.robots.Check(ctx, c.requester, parsed)
		if robotsErr != nil {
			var ge *GovernanceError
			if errors.As(robotsErr, &ge) && ge.Code == ErrCodeRateLimited {
				c.governor.RecordBlock(host, ge)
			}
			return acquisition.Response{}, robotsErr
		}
		crawlDelay = delay
	}

	if err := c.governor.AllowAndWait(ctx, host, crawlDelay); err != nil {
		return acquisition.Response{}, err
	}

	resp, err := c.requester.FetchRequest(ctx, opts)
	if err != nil {
		if errors.Is(err, acquisition.ErrNotAllowed) {
			return acquisition.Response{}, &GovernanceError{Code: ErrCodeSSRFBlocked, Message: err.Error(), Host: host, Err: err}
		}
		if strings.Contains(err.Error(), "exceeds") && strings.Contains(err.Error(), "byte limit") {
			return resp, &GovernanceError{Code: ErrCodeBodyTooLarge, Message: err.Error(), Host: host, Err: err}
		}
		return resp, err
	}

	if ct := resp.Receipt.Headers["content-type"]; ct != "" {
		if ctErr := ValidateContentType(ct); ctErr != nil {
			if ge, ok := ctErr.(*GovernanceError); ok {
				ge.Host = host
			}
			return acquisition.Response{}, ctErr
		}
	}

	if int64(len(resp.Body)) > c.cfg.MaxBodyBytes {
		return acquisition.Response{}, &GovernanceError{
			Code:    ErrCodeBodyTooLarge,
			Message: fmt.Sprintf("response body %d exceeds %d byte cap", len(resp.Body), c.cfg.MaxBodyBytes),
			Host:    host,
		}
	}

	if ge := ClassifyResponse(resp, host); ge != nil {
		c.governor.RecordBlock(host, ge)
		return resp, ge
	}
	c.governor.RecordSuccess(host)
	return resp, nil
}

func (c *SafeClient) Render(ctx context.Context, req RenderRequest) (RenderResult, error) {
	parsed, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || parsed.Hostname() == "" {
		return RenderResult{}, &GovernanceError{Code: ErrCodeSSRFBlocked, Message: "invalid HTTP(S) target URL", Err: err}
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if err := checkAllowedDomains(host, req.AllowedDomains); err != nil {
		return RenderResult{}, err
	}
	if c.cfg.RespectRobots {
		if _, err := c.robots.Check(ctx, c.requester, parsed); err != nil {
			return RenderResult{}, err
		}
	}
	if err := c.governor.AllowAndWait(ctx, host, 0); err != nil {
		return RenderResult{}, err
	}
	if c.cfg.Renderer == nil {
		return RenderResult{}, &GovernanceError{
			Code:    ErrCodeRendererUnavailable,
			Message: "external CDP sidecar renderer is not configured",
			Host:    host,
		}
	}
	res, err := c.cfg.Renderer.RenderAndSniff(ctx, req)
	if err != nil {
		return res, err
	}
	if res.BlockedReason != "" {
		ge := &GovernanceError{Code: ErrCodeChallengeBlocked, Message: res.BlockedReason, Host: host}
		c.governor.RecordBlock(host, ge)
		return res, ge
	}
	c.governor.RecordSuccess(host)
	return res, nil
}

func (c *SafeClient) applyAuthProfile(name, host string, opts *acquisition.RequestOptions) error {
	c.mu.Lock()
	profile, ok := c.profiles[name]
	c.mu.Unlock()
	if !ok {
		return &GovernanceError{Code: ErrCodeAuthRequired, Message: fmt.Sprintf("unknown auth_profile %q", name), Host: host}
	}
	domainOK := len(profile.AllowedDomains) == 0
	for _, d := range profile.AllowedDomains {
		ld := strings.ToLower(strings.TrimSpace(d))
		if host == ld || strings.HasSuffix(host, "."+ld) {
			domainOK = true
			break
		}
	}
	if !domainOK {
		return &GovernanceError{
			Code:    ErrCodeAuthRequired,
			Message: fmt.Sprintf("auth_profile %q is not permitted for host %q", name, host),
			Host:    host,
		}
	}
	secret := profile.Secret
	if secret == "" && profile.EnvVar != "" {
		secret = os.Getenv(profile.EnvVar)
	}
	if secret == "" {
		return &GovernanceError{
			Code:    ErrCodeAuthRequired,
			Message: fmt.Sprintf("auth_profile %q has no secret configured in %s", name, profile.EnvVar),
			Host:    host,
		}
	}
	if opts.Headers == nil {
		opts.Headers = make(map[string]string)
	}
	if profile.HeaderName != "" {
		opts.Headers[profile.HeaderName] = profile.HeaderPrefix + secret
	}
	if profile.QueryParam != "" {
		u, err := url.Parse(opts.URL)
		if err != nil {
			return err
		}
		q := u.Query()
		q.Set(profile.QueryParam, secret)
		u.RawQuery = q.Encode()
		opts.URL = u.String()
	}
	return nil
}

// HTTPPageRenderer connects to an optional external CDP/Playwright Sidecar with
// strict concurrency bounding via semaphore.
type HTTPPageRenderer struct {
	endpoint string
	token    string
	client   *http.Client
	slots    chan struct{}
}

func NewHTTPPageRenderer(endpoint, token string, maxConcurrent int) (*HTTPPageRenderer, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("sidecar endpoint is required")
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	return &HTTPPageRenderer{
		endpoint: endpoint,
		token:    token,
		client:   &http.Client{Timeout: 30 * time.Second},
		slots:    make(chan struct{}, maxConcurrent),
	}, nil
}

func (r *HTTPPageRenderer) RenderAndSniff(ctx context.Context, in RenderRequest) (RenderResult, error) {
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return RenderResult{}, ctx.Err()
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return RenderResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint+"/render", bytes.NewReader(payload))
	if err != nil {
		return RenderResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return RenderResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return RenderResult{}, fmt.Errorf("sidecar renderer returned HTTP %d", resp.StatusCode)
	}
	var out RenderResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return RenderResult{}, err
	}
	return out, nil
}

func isGovernanceBlock(err error) bool {
	var ge *GovernanceError
	return errors.As(err, &ge)
}

func checkAllowedDomains(host string, allowed []string) error {
	if len(allowed) == 0 {
		return nil
	}
	for _, d := range allowed {
		ld := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
		if ld == "" {
			continue
		}
		if host == ld || strings.HasSuffix(host, "."+ld) {
			return nil
		}
	}
	return &GovernanceError{
		Code:    ErrCodeSSRFBlocked,
		Message: fmt.Sprintf("host %q is outside granted allowed_domains", host),
		Host:    host,
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
