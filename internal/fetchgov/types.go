package fetchgov

import (
	"context"
	"fmt"
	"time"

	"gorelay/internal/acquisition"
)

const (
	DefaultMaxBodyBytes    int64   = 5 * 1024 * 1024
	DefaultRobotsTTL               = time.Hour
	DefaultRequestsPerSec  float64 = 2.0
	DefaultBurst                   = 4
	DefaultCircuitThreshold        = 2
	DefaultCircuitCooldown         = 60 * time.Second
)

const (
	ErrCodeSSRFBlocked         = "SSRF_BLOCKED"
	ErrCodeRobotsDisallowed    = "ROBOTS_DISALLOWED"
	ErrCodeRateLimited         = "RATE_LIMITED"
	ErrCodeChallengeBlocked    = "CHALLENGE_BLOCKED"
	ErrCodeAuthRequired        = "AUTH_REQUIRED"
	ErrCodeCircuitOpen         = "CIRCUIT_OPEN"
	ErrCodeContentTypeRejected = "CONTENT_TYPE_REJECTED"
	ErrCodeBodyTooLarge        = "BODY_TOO_LARGE"
	ErrCodeRendererUnavailable = "RENDERER_UNAVAILABLE"
)

// GovernanceError describes a policy, rate-limit, challenge, or safety block.
type GovernanceError struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Host          string `json:"host,omitempty"`
	StatusCode    int    `json:"status_code,omitempty"`
	RetryAfterSec int    `json:"retry_after_sec,omitempty"`
	Err           error  `json:"-"`
}

func (e *GovernanceError) Error() string {
	if e.RetryAfterSec > 0 {
		return fmt.Sprintf("%s (%s): retry after %ds", e.Code, e.Message, e.RetryAfterSec)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *GovernanceError) Unwrap() error { return e.Err }

// AuthProfile configures legitimate connector credentials without storing secrets
// inside connector definitions or logs.
type AuthProfile struct {
	Name           string   `json:"name"`
	AllowedDomains []string `json:"allowed_domains"`
	HeaderName     string   `json:"header_name,omitempty"`
	HeaderPrefix   string   `json:"header_prefix,omitempty"`
	QueryParam     string   `json:"query_param,omitempty"`
	EnvVar         string   `json:"env_var,omitempty"`
	Secret         string   `json:"-"` // Optional in-memory override for tests; never serialized.
}

// Requester executes governed HTTP requests using verified public network rules.
type Requester interface {
	FetchRequest(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error)
	Close()
}

// PageRenderer optionally renders dynamic SPA pages via an external Sidecar.
type PageRenderer interface {
	RenderAndSniff(ctx context.Context, req RenderRequest) (RenderResult, error)
}

type RenderRequest struct {
	URL            string            `json:"url"`
	WaitSelector   string            `json:"wait_selector,omitempty"`
	WaitMS         int               `json:"wait_ms,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	AllowedDomains []string          `json:"allowed_domains,omitempty"`
}

type RenderResult struct {
	FinalURL      string                          `json:"final_url"`
	HTML          string                          `json:"html"`
	NetworkAPIs   []acquisition.NetworkAPICapture `json:"network_apis,omitempty"`
	BlockedReason string                          `json:"blocked_reason,omitempty"`
}

type FetchRequest struct {
	URL           string
	Method        string
	Headers       map[string]string
	Body          string
	AuthProfile   string
	AllowedDomains []string
}
