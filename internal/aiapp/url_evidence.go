package aiapp

import (
	"fmt"
	"net/url"

	"go-lua-crawler/internal/acquisition"
)

// This is a declared, deterministic transformation of an observed value.
// It cannot accept an arbitrary replacement value or executable code.
type URLTransform struct {
	QueryParameter string `json:"query_parameter,omitempty"`
	Resolve        bool   `json:"resolve,omitempty"`
}

func TransformObservedURL(value any, source acquisition.Response, transform URLTransform) (string, error) {
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("URL_TRANSFORM_INVALID: URL transformation requires an observed string")
	}
	if transform.QueryParameter == "" && !transform.Resolve {
		return "", fmt.Errorf("URL_TRANSFORM_INVALID: declare resolve=true or an observed query_parameter")
	}
	if transform.QueryParameter != "" {
		decoded, err := acquisition.URLQueryValue(raw, transform.QueryParameter)
		if err != nil {
			return "", fmt.Errorf("URL_QUERY_PARAMETER_INVALID: %w", err)
		}
		raw = decoded
	}
	target, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("URL_TRANSFORM_INVALID: %w", err)
	}
	if transform.Resolve {
		base, err := url.Parse(source.Receipt.URL)
		if err != nil {
			return "", fmt.Errorf("URL_TRANSFORM_INVALID: %w", err)
		}
		target = base.ResolveReference(target)
	}
	if target.User != nil || target.Hostname() == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return "", fmt.Errorf("URL_TRANSFORM_INVALID: transformed value must be an absolute HTTP(S) URL without credentials")
	}
	return target.String(), nil
}
