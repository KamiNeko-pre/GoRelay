package acquisition

import (
	"fmt"
	"net/url"
)

// URLQueryValue decodes one observed URL parameter, including percent escapes.
// Duplicate parameters are ambiguous and never selected silently.
func URLQueryValue(raw, key string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", fmt.Errorf("query parameter name is required")
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", err
	}
	found := values[key]
	if len(found) != 1 || found[0] == "" {
		return "", fmt.Errorf("observed URL requires exactly one nonempty query parameter %q", key)
	}
	return found[0], nil
}
