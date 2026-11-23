package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
)

var proxyAliasRange = netip.MustParsePrefix("198.18.0.0/15")

func (f *PublicFetcher) resolveIPs(ctx context.Context, host string) ([]net.IP, error) {
	ips, err := f.lookupIP(ctx, "ip", host)
	if err != nil || f.proxyURL == nil || !proxyAliases(ips) {
		return ips, err
	}
	// Fake-IP answers identify a route in the local proxy, not the origin.
	// Resolve through that proxy, then let publicDial validate and pin the real IPs.
	return f.resolveProxyAliases(ctx, host)
}

func proxyAliases(ips []net.IP) bool {
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok || !proxyAliasRange.Contains(addr.Unmap()) {
			return false
		}
	}
	return true
}

func (f *PublicFetcher) resolveProxyAliases(ctx context.Context, host string) ([]net.IP, error) {
	transport := &http.Transport{Proxy: http.ProxyURL(f.proxyURL), DialContext: f.dial, TLSHandshakeTimeout: f.policy.Timeout, ResponseHeaderTimeout: f.policy.Timeout}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: f.policy.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var ips []net.IP
	for _, kind := range []string{"A", "AAAA"} {
		query := url.Values{"name": {host}, "type": {kind}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://dns.google/resolve?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("resolve proxy DNS alias for %s: %w", host, err)
		}
		var answer struct {
			Status int
			Answer []struct {
				Type int
				Data string
			}
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&answer)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("proxy DNS lookup for %s returned HTTP %d", host, resp.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode proxy DNS reply for %s: %w", host, decodeErr)
		}
		if answer.Status != 0 {
			return nil, fmt.Errorf("proxy DNS lookup for %s returned DNS status %d", host, answer.Status)
		}
		for _, record := range answer.Answer {
			if record.Type != 1 && record.Type != 28 {
				continue
			}
			ip := net.ParseIP(record.Data)
			if ip == nil {
				return nil, fmt.Errorf("proxy DNS returned an invalid IP for %s", host)
			}
			ips = append(ips, ip)
		}
	}
	return ips, nil
}
