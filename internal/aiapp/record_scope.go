package aiapp

import (
	"net/url"
	"strings"
)

func recordInScope(row Record, domains []string) bool {
	if len(domains) == 0 {
		return true
	}
	u, err := url.Parse(row.Provenance.URL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}
