package knowledge

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// NormalizeInput prepares full-text input without choosing a document identity
// or revision. Paragraphs and code indentation are part of the evidence.
func NormalizeInput(input Input) (Input, error) {
	if len(input.IdentityNamespace) > 128 || strings.TrimSpace(input.IdentityNamespace) != input.IdentityNamespace {
		return Input{}, fmt.Errorf("invalid host identity namespace")
	}
	input.Source = strings.TrimSpace(input.Source)
	input.SourceKey = strings.TrimSpace(input.SourceKey)
	input.URL = strings.TrimSpace(input.URL)
	input.Title = strings.TrimSpace(input.Title)
	if input.Source == "" || len(input.Source) > 128 || input.SourceKey == "" || len(input.SourceKey) > 1024 {
		return Input{}, fmt.Errorf("source and source_key are required (maximum 128 and 1024 bytes)")
	}
	if input.Format != "text" && input.Format != "markdown" {
		return Input{}, fmt.Errorf("format must be text or markdown; HTML must be extracted first")
	}
	if len(input.Body) > MaxBodyBytes || !utf8.ValidString(input.Body) {
		return Input{}, fmt.Errorf("body must be valid UTF-8 and at most %d bytes", MaxBodyBytes)
	}
	input.Body = strings.ReplaceAll(strings.ReplaceAll(input.Body, "\r\n", "\n"), "\r", "\n")
	// Remove only blank boundary lines. Trimming the whole string would erase
	// meaningful indentation in the first code line or trailing code spaces.
	lines := strings.Split(input.Body, "\n")
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	input.Body = strings.Join(lines[start:end], "\n")
	if input.Body == "" {
		return Input{}, fmt.Errorf("body is required")
	}
	if input.Title == "" && input.Format == "markdown" {
		for _, line := range strings.Split(input.Body, "\n") {
			if strings.HasPrefix(line, "# ") {
				input.Title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
				break
			}
		}
	}
	if input.Title == "" || len(input.Title) > 512 || !utf8.ValidString(input.Title) {
		return Input{}, fmt.Errorf("title is required and must be valid UTF-8, at most 512 bytes")
	}
	if input.URL != "" {
		parsed, err := url.Parse(input.URL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
			return Input{}, fmt.Errorf("url must be an absolute HTTP(S) URL without credentials")
		}
	}
	return input, nil
}
