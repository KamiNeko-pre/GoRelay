package fetchgov

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorelay/internal/acquisition"
)

type robotsRule struct {
	allow bool
	path  string
}

type robotsPolicy struct {
	rules      []robotsRule
	crawlDelay time.Duration
	expiresAt  time.Time
}

func (p robotsPolicy) allowed(path string) bool {
	if path == "" {
		path = "/"
	}
	bestLen := -1
	allowed := true
	for _, r := range p.rules {
		if r.path == "" {
			continue
		}
		if strings.HasPrefix(path, r.path) && len(r.path) >= bestLen {
			if len(r.path) > bestLen || r.allow {
				bestLen = len(r.path)
				allowed = r.allow
			}
		}
	}
	return allowed
}

// ParseRobots parses robots.txt according to RFC 9309 group matching, preferring
// the specific agent group before falling back to "*".
func ParseRobots(content, agent string) robotsPolicy {
	agent = strings.ToLower(strings.TrimSpace(agent))
	type group struct {
		agents []string
		rules  []robotsRule
		delay  time.Duration
	}
	var groups []*group
	var current *group
	sawRuleInGroup := false

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		switch key {
		case "user-agent":
			if current == nil || sawRuleInGroup {
				current = &group{}
				groups = append(groups, current)
				sawRuleInGroup = false
			}
			current.agents = append(current.agents, strings.ToLower(val))
		case "disallow":
			if current == nil {
				continue
			}
			sawRuleInGroup = true
			if val != "" {
				current.rules = append(current.rules, robotsRule{allow: false, path: val})
			}
		case "allow":
			if current == nil {
				continue
			}
			sawRuleInGroup = true
			if val != "" {
				current.rules = append(current.rules, robotsRule{allow: true, path: val})
			}
		case "crawl-delay":
			if current == nil {
				continue
			}
			sawRuleInGroup = true
			if sec, err := strconv.ParseFloat(val, 64); err == nil && sec > 0 && sec <= 60 {
				current.delay = time.Duration(sec * float64(time.Second))
			}
		}
	}

	var matched, wildcard *group
	for _, g := range groups {
		for _, a := range g.agents {
			if agent != "" && strings.HasPrefix(agent, a) && a != "*" {
				if matched == nil {
					matched = g
				}
			}
			if a == "*" && wildcard == nil {
				wildcard = g
			}
		}
	}
	chosen := matched
	if chosen == nil {
		chosen = wildcard
	}
	if chosen == nil {
		return robotsPolicy{}
	}
	return robotsPolicy{rules: chosen.rules, crawlDelay: chosen.delay}
}

type RobotsCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	agent   string
	entries map[string]robotsPolicy
	now     func() time.Time
}

func NewRobotsCache(ttl time.Duration, agent string) *RobotsCache {
	if ttl <= 0 {
		ttl = DefaultRobotsTTL
	}
	if agent == "" {
		agent = "gorelay"
	}
	return &RobotsCache{
		ttl:     ttl,
		maxSize: 512,
		agent:   agent,
		entries: make(map[string]robotsPolicy),
		now:     time.Now,
	}
}

func (c *RobotsCache) Check(ctx context.Context, req Requester, target *url.URL) (time.Duration, error) {
	if target == nil || req == nil {
		return 0, nil
	}
	hostKey := strings.ToLower(target.Scheme + "://" + target.Host)
	now := c.now()

	c.mu.Lock()
	pol, ok := c.entries[hostKey]
	c.mu.Unlock()

	if !ok || now.After(pol.expiresAt) {
		robotsURL := hostKey + "/robots.txt"
		resp, err := req.FetchRequest(ctx, acquisition.RequestOptions{
			Method: http.MethodGet,
			URL:    robotsURL,
		})
		if err != nil {
			// Transport errors during robots lookup do not panic; treat transient fetch error conservatively or surface SSRF.
			if isGovernanceBlock(err) {
				return 0, err
			}
			pol = robotsPolicy{expiresAt: now.Add(5 * time.Minute)}
		} else if resp.Receipt.Status == http.StatusTooManyRequests {
			return 0, &GovernanceError{
				Code:       ErrCodeRateLimited,
				Message:    fmt.Sprintf("rate limited fetching %s", robotsURL),
				Host:       strings.ToLower(target.Hostname()),
				StatusCode: resp.Receipt.Status,
			}
		} else if resp.Receipt.Status >= 200 && resp.Receipt.Status < 300 {
			pol = ParseRobots(resp.Body, c.agent)
			pol.expiresAt = now.Add(c.ttl)
		} else {
			// RFC 9309: 4xx (except 429) grants full access; cache briefly.
			pol = robotsPolicy{expiresAt: now.Add(c.ttl)}
		}

		c.mu.Lock()
		if len(c.entries) >= c.maxSize {
			clear(c.entries)
		}
		c.entries[hostKey] = pol
		c.mu.Unlock()
	}

	reqPath := target.EscapedPath()
	if reqPath == "" {
		reqPath = "/"
	}
	if target.RawQuery != "" {
		reqPath += "?" + target.RawQuery
	}
	if !pol.allowed(reqPath) {
		return pol.crawlDelay, &GovernanceError{
			Code:    ErrCodeRobotsDisallowed,
			Message: fmt.Sprintf("path %q disallowed by %s/robots.txt", target.Path, hostKey),
			Host:    strings.ToLower(target.Hostname()),
		}
	}
	return pol.crawlDelay, nil
}
