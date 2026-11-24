package fetchgov

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"gorelay/internal/acquisition"
)

type domainState struct {
	limiter       *rate.Limiter
	crawlDelay    time.Duration
	failures      int
	openUntil     time.Time
	lastErrorCode string
}

// DomainGovernor coordinates per-host token bucket rate limiting and circuit breaking.
type DomainGovernor struct {
	mu               sync.Mutex
	rps              float64
	burst            int
	failureThreshold int
	cooldown         time.Duration
	hosts            map[string]*domainState
	now              func() time.Time
}

func NewDomainGovernor(rps float64, burst, threshold int, cooldown time.Duration) *DomainGovernor {
	if rps <= 0 {
		rps = DefaultRequestsPerSec
	}
	if burst <= 0 {
		burst = DefaultBurst
	}
	if threshold <= 0 {
		threshold = DefaultCircuitThreshold
	}
	if cooldown <= 0 {
		cooldown = DefaultCircuitCooldown
	}
	return &DomainGovernor{
		rps:              rps,
		burst:            burst,
		failureThreshold: threshold,
		cooldown:         cooldown,
		hosts:            make(map[string]*domainState),
		now:              time.Now,
	}
}

func (g *DomainGovernor) stateLocked(host string, crawlDelay time.Duration) *domainState {
	host = strings.ToLower(strings.TrimSpace(host))
	st, ok := g.hosts[host]
	if !ok {
		effectiveRPS := g.rps
		burst := g.burst
		if crawlDelay > 0 {
			delayRPS := 1.0 / crawlDelay.Seconds()
			if delayRPS < effectiveRPS {
				effectiveRPS = delayRPS
				burst = 1
			}
		}
		st = &domainState{
			limiter:    rate.NewLimiter(rate.Limit(effectiveRPS), burst),
			crawlDelay: crawlDelay,
		}
		g.hosts[host] = st
		return st
	}
	if crawlDelay > 0 && crawlDelay != st.crawlDelay {
		effectiveRPS := min(g.rps, 1.0/crawlDelay.Seconds())
		burst := g.burst
		if 1.0/crawlDelay.Seconds() < g.rps {
			burst = 1
		}
		st.limiter.SetLimit(rate.Limit(effectiveRPS))
		st.limiter.SetBurst(burst)
		st.crawlDelay = crawlDelay
	}
	return st
}

func (g *DomainGovernor) AllowAndWait(ctx context.Context, host string, crawlDelay time.Duration) error {
	now := g.now()
	g.mu.Lock()
	st := g.stateLocked(host, crawlDelay)
	openUntil := st.openUntil
	reason := st.lastErrorCode
	limiter := st.limiter
	g.mu.Unlock()

	if now.Before(openUntil) {
		sec := int(math.Ceil(openUntil.Sub(now).Seconds()))
		if sec < 1 {
			sec = 1
		}
		return &GovernanceError{
			Code:          ErrCodeCircuitOpen,
			Message:       fmt.Sprintf("circuit open for %s after %s", host, reason),
			Host:          host,
			RetryAfterSec: sec,
		}
	}
	if err := limiter.Wait(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		retrySec := int(math.Ceil(crawlDelay.Seconds()))
		if retrySec < 1 {
			retrySec = 1
		}
		return &GovernanceError{
			Code:          ErrCodeRateLimited,
			Message:       fmt.Sprintf("per-domain rate limit or robots Crawl-delay for %s exceeds request deadline", host),
			Host:          host,
			RetryAfterSec: retrySec,
			Err:           err,
		}
	}
	return nil
}

func (g *DomainGovernor) RecordSuccess(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st, ok := g.hosts[strings.ToLower(strings.TrimSpace(host))]; ok {
		st.failures = 0
		st.openUntil = time.Time{}
		st.lastErrorCode = ""
	}
}

func (g *DomainGovernor) RecordBlock(host string, ge *GovernanceError) {
	if ge == nil {
		return
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.stateLocked(host, 0)
	st.failures++
	st.lastErrorCode = ge.Code

	// Immediate stop on anti-bot challenge or explicit Retry-After / 429 threshold.
	immediate := ge.Code == ErrCodeChallengeBlocked || ge.RetryAfterSec > 0
	if immediate || st.failures >= g.failureThreshold {
		wait := g.cooldown
		if d := time.Duration(ge.RetryAfterSec) * time.Second; d > wait {
			wait = d
		}
		st.openUntil = now.Add(wait)
	}
}

func ValidateContentType(contentType string) error {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if ct == "" {
		return nil
	}
	media, _, _ := strings.Cut(ct, ";")
	media = strings.TrimSpace(media)
	if strings.HasPrefix(media, "text/") ||
		media == "application/json" ||
		strings.HasSuffix(media, "+json") ||
		media == "application/xml" ||
		strings.HasSuffix(media, "+xml") ||
		media == "application/javascript" ||
		media == "application/xhtml+xml" {
		return nil
	}
	return &GovernanceError{
		Code:    ErrCodeContentTypeRejected,
		Message: fmt.Sprintf("unsupported non-text content-type %q", media),
	}
}

func ClassifyResponse(resp acquisition.Response, host string) *GovernanceError {
	status := resp.Receipt.Status
	hdrs := resp.Receipt.Headers

	if status == http.StatusTooManyRequests {
		retrySec := parseRetryAfter(hdrs["retry-after"], time.Now())
		return &GovernanceError{
			Code:          ErrCodeRateLimited,
			Message:       "upstream returned HTTP 429 Too Many Requests",
			Host:          host,
			StatusCode:    status,
			RetryAfterSec: retrySec,
		}
	}

	if hasChallengeHeaders(hdrs) || (isChallengeStatus(status) && looksLikeChallengeBody(resp.Body)) {
		return &GovernanceError{
			Code:       ErrCodeChallengeBlocked,
			Message:    fmt.Sprintf("upstream returned HTTP %d anti-bot/WAF challenge", status),
			Host:       host,
			StatusCode: status,
		}
	}

	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &GovernanceError{
			Code:       ErrCodeAuthRequired,
			Message:    fmt.Sprintf("upstream returned HTTP %d unauthorized/forbidden", status),
			Host:       host,
			StatusCode: status,
		}
	}
	return nil
}

func parseRetryAfter(raw string, now time.Time) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
		return sec
	}
	if when, err := http.ParseTime(raw); err == nil && when.After(now) {
		return int(math.Ceil(when.Sub(now).Seconds()))
	}
	return 0
}

func isChallengeStatus(status int) bool {
	return status == http.StatusForbidden || status == http.StatusServiceUnavailable || status == 202
}

func hasChallengeHeaders(hdrs map[string]string) bool {
	for k, v := range hdrs {
		lk := strings.ToLower(k)
		lv := strings.ToLower(v)
		if lk == "cf-mitigated" && strings.Contains(lv, "challenge") {
			return true
		}
		if lk == "x-amzn-waf-action" && strings.Contains(lv, "captcha") {
			return true
		}
	}
	return false
}

func looksLikeChallengeBody(body string) bool {
	prefix := strings.ToLower(body)
	if len(prefix) > 8192 {
		prefix = prefix[:8192]
	}
	markers := []string{
		"cf-browser-verification",
		"attention required! | cloudflare",
		"challenges.cloudflare.com/turnstile",
		"g-recaptcha",
		"hcaptcha.com/1/api.js",
		"captcha-delivery.com",
		"verify you are human",
		"enable javascript and cookies to continue",
	}
	for _, m := range markers {
		if strings.Contains(prefix, m) {
			return true
		}
	}
	return false
}
