package fetchgov

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorelay/internal/acquisition"
)

type stubRequester struct {
	fn func(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error)
}

func (s *stubRequester) FetchRequest(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
	return s.fn(ctx, opts)
}
func (s *stubRequester) Close() {}

type stubRenderer struct {
	res RenderResult
	err error
}

func (r *stubRenderer) RenderAndSniff(context.Context, RenderRequest) (RenderResult, error) {
	return r.res, r.err
}

func TestSafeClientBlocksSSRFAndNonPublicURLs(t *testing.T) {
	client, err := NewSafeClient(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, target := range []string{
		"http://127.0.0.1:8080/admin",
		"http://localhost/metrics",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/secret",
		"file:///etc/passwd",
	} {
		_, err := client.Fetch(context.Background(), FetchRequest{URL: target})
		var ge *GovernanceError
		if !errors.As(err, &ge) || ge.Code != ErrCodeSSRFBlocked {
			t.Fatalf("url %q error = %v, want SSRF_BLOCKED", target, err)
		}
	}
}

func TestSafeClientEnforcesRobotsAndCircuitBreaker(t *testing.T) {
	var hits atomic.Int32
	cfg := DefaultConfig()
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.CircuitCooldown = 10 * time.Second
	cfg.RequesterFactory = func(acquisition.Policy) (Requester, error) {
		return &stubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			if strings.HasSuffix(opts.URL, "/robots.txt") {
				return acquisition.Response{
					Body:    "User-agent: *\nDisallow: /private/\nAllow: /private/public-docs\n",
					Receipt: acquisition.Receipt{Status: 200, Headers: map[string]string{"content-type": "text/plain"}},
				}, nil
			}
			hits.Add(1)
			if strings.Contains(opts.URL, "/limit") {
				return acquisition.Response{
					Body: "too many requests",
					Receipt: acquisition.Receipt{
						Status:  429,
						Headers: map[string]string{"retry-after": "30", "content-type": "text/plain"},
					},
				}, nil
			}
			return acquisition.Response{
				Body:    `{"ok":true}`,
				Receipt: acquisition.Receipt{Status: 200, Headers: map[string]string{"content-type": "application/json"}},
			}, nil
		}}, nil
	}

	client, err := NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.Fetch(context.Background(), FetchRequest{URL: "https://example.org/private/secret"}); err == nil {
		t.Fatal("disallowed robots path succeeded")
	} else if ge := err.(*GovernanceError); ge.Code != ErrCodeRobotsDisallowed {
		t.Fatalf("robots error = %v", ge)
	}

	resp, err := client.Fetch(context.Background(), FetchRequest{URL: "https://example.org/private/public-docs"})
	if err != nil || resp.Body != `{"ok":true}` {
		t.Fatalf("allowed Allow-override path failed: resp=%+v err=%v", resp, err)
	}

	_, err = client.Fetch(context.Background(), FetchRequest{URL: "https://example.org/limit"})
	if ge := err.(*GovernanceError); ge == nil || ge.Code != ErrCodeRateLimited || ge.RetryAfterSec != 30 {
		t.Fatalf("expected RATE_LIMITED with retry-after=30, got %v", err)
	}

	before := hits.Load()
	_, err = client.Fetch(context.Background(), FetchRequest{URL: "https://example.org/private/public-docs"})
	if ge := err.(*GovernanceError); ge == nil || ge.Code != ErrCodeCircuitOpen {
		t.Fatalf("expected CIRCUIT_OPEN after 429, got %v", err)
	}
	if hits.Load() != before {
		t.Fatal("circuit open still hit upstream requester")
	}
}

func TestSafeClientRejectsBinaryContentOversizedAuthMismatchAndChallenge(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RespectRobots = false
	cfg.MaxBodyBytes = 64
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.AuthProfiles = map[string]AuthProfile{
		"gh": {Name: "gh", AllowedDomains: []string{"api.github.com"}, HeaderName: "Authorization", HeaderPrefix: "Bearer ", Secret: "secret-tok"},
	}
	cfg.Renderer = &stubRenderer{res: RenderResult{BlockedReason: "cloudflare turnstile"}}
	cfg.RequesterFactory = func(acquisition.Policy) (Requester, error) {
		return &stubRequester{fn: func(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
			switch {
			case strings.Contains(opts.URL, "/zip"):
				return acquisition.Response{Body: "PK...", Receipt: acquisition.Receipt{Status: 200, Headers: map[string]string{"content-type": "application/zip"}}}, nil
			case strings.Contains(opts.URL, "/big"):
				return acquisition.Response{Body: strings.Repeat("a", 128), Receipt: acquisition.Receipt{Status: 200, Headers: map[string]string{"content-type": "text/plain"}}}, nil
			case strings.Contains(opts.URL, "/cf"):
				return acquisition.Response{Body: "<html>Attention Required! | Cloudflare</html>", Receipt: acquisition.Receipt{Status: 403, Headers: map[string]string{"content-type": "text/html"}}}, nil
			default:
				if opts.Headers["Authorization"] != "Bearer secret-tok" {
					return acquisition.Response{Receipt: acquisition.Receipt{Status: http.StatusUnauthorized}}, nil
				}
				return acquisition.Response{Body: `{"ok":true}`, Receipt: acquisition.Receipt{Status: 200, Headers: map[string]string{"content-type": "application/json"}}}, nil
			}
		}}, nil
	}

	client, err := NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Fetch(context.Background(), FetchRequest{URL: "https://example.org/zip"}); err.(*GovernanceError).Code != ErrCodeContentTypeRejected {
		t.Fatalf("zip unexpected err: %v", err)
	}
	if _, err := client.Fetch(context.Background(), FetchRequest{URL: "https://example.org/big"}); err.(*GovernanceError).Code != ErrCodeBodyTooLarge {
		t.Fatalf("oversized unexpected err: %v", err)
	}
	if _, err := client.Fetch(context.Background(), FetchRequest{URL: "https://evil.org/api", AuthProfile: "gh"}); err.(*GovernanceError).Code != ErrCodeAuthRequired {
		t.Fatalf("foreign domain received auth profile")
	}
	if _, err := client.Fetch(context.Background(), FetchRequest{URL: "https://api.github.com/repos", AuthProfile: "gh"}); err != nil {
		t.Fatalf("authorized domain failed: %v", err)
	}
	if _, err := client.Fetch(context.Background(), FetchRequest{URL: "https://example.org/cf"}); err.(*GovernanceError).Code != ErrCodeChallengeBlocked {
		t.Fatalf("expected challenge block, got %v", err)
	}
	if _, err := client.Render(context.Background(), RenderRequest{URL: "https://spa.example.com"}); err.(*GovernanceError).Code != ErrCodeChallengeBlocked {
		t.Fatalf("expected renderer challenge block, got %v", err)
	}
}
