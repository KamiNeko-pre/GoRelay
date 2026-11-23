package acquisition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EmptyResultEvidence names actual source structure, not a model assertion of absence.
type EmptyResultEvidence struct {
	JSONPointer   *string `json:"json_pointer,omitempty"`
	RowsSelector  string  `json:"rows_selector,omitempty"`
	EmptySelector string  `json:"empty_selector,omitempty"`
	EmptyText     string  `json:"empty_text,omitempty"`
}
type SourceCheckEvidence struct {
	URL          string              `json:"url"`
	SourceDigest string              `json:"source_digest"`
	Locator      EmptyResultEvidence `json:"locator"`
}
type SourceCheck struct {
	ReceiptID string                `json:"receipt_id"`
	Outcome   string                `json:"outcome"`
	URLs      []string              `json:"urls"`
	CheckedAt time.Time             `json:"checked_at"`
	Meaning   string                `json:"meaning"`
	Evidence  []SourceCheckEvidence `json:"evidence,omitempty"`
}

// CheckEmptySource checks an immutable successful GET. A missing selector, null,
// HTTP failure or empty script output cannot become a no-match observation.
func CheckEmptySource(source Response, locator EmptyResultEvidence) (SourceCheck, error) {
	r := source.Receipt
	u, err := url.Parse(r.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || r.Method != "GET" || r.Status < 200 || r.Status >= 300 || r.FetchedAt.IsZero() {
		return SourceCheck{}, fmt.Errorf("source_check requires an actual successful GET receipt with URL and fetched_at")
	}
	if locator.JSONPointer != nil {
		if locator.RowsSelector != "" || locator.EmptySelector != "" || locator.EmptyText != "" {
			return SourceCheck{}, fmt.Errorf("choose JSON collection evidence or DOM empty-state evidence, not both")
		}
		var value any
		if err := json.Unmarshal([]byte(source.Body), &value); err != nil {
			return SourceCheck{}, fmt.Errorf("source_check response is not JSON: %w", err)
		}
		path := *locator.JSONPointer
		if path != "" {
			if !strings.HasPrefix(path, "/") {
				return SourceCheck{}, fmt.Errorf("source_check.json_pointer must be empty or start with /")
			}
			for _, raw := range strings.Split(path[1:], "/") {
				key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
				switch v := value.(type) {
				case map[string]any:
					var ok bool
					value, ok = v[key]
					if !ok {
						return SourceCheck{}, fmt.Errorf("source_check.json_pointer %q has no member %q", path, key)
					}
				case []any:
					i, e := strconv.Atoi(key)
					if e != nil || i < 0 || i >= len(v) {
						return SourceCheck{}, fmt.Errorf("source_check.json_pointer %q has no array item %q", path, key)
					}
					value = v[i]
				default:
					return SourceCheck{}, fmt.Errorf("source_check.json_pointer %q crosses a scalar", path)
				}
			}
		}
		rows, ok := value.([]any)
		if !ok || len(rows) != 0 {
			return SourceCheck{}, fmt.Errorf("source_check.json_pointer must select an existing empty array; missing/null/nonempty values are not no-match evidence")
		}
	} else {
		if strings.TrimSpace(locator.RowsSelector) == "" || strings.TrimSpace(locator.EmptySelector) == "" || strings.TrimSpace(locator.EmptyText) == "" {
			return SourceCheck{}, fmt.Errorf("DOM source_check requires rows_selector, empty_selector and the exact observed empty_text; zero selector matches alone are a parsing gap")
		}
		rows, e := cascadia.Compile(locator.RowsSelector)
		if e != nil {
			return SourceCheck{}, fmt.Errorf("source_check.rows_selector: %w", e)
		}
		marker, e := cascadia.Compile(locator.EmptySelector)
		if e != nil {
			return SourceCheck{}, fmt.Errorf("source_check.empty_selector: %w", e)
		}
		doc, e := goquery.NewDocumentFromReader(strings.NewReader(source.Body))
		if e != nil {
			return SourceCheck{}, e
		}
		doc.Find("script,style,noscript,template").Remove()
		if doc.FindMatcher(rows).Length() != 0 {
			return SourceCheck{}, fmt.Errorf("source_check.rows_selector still matches records; extract them instead of reporting no match")
		}
		found := false
		quote := strings.Join(strings.Fields(locator.EmptyText), " ")
		doc.FindMatcher(marker).Each(func(_ int, n *goquery.Selection) {
			if strings.Join(strings.Fields(n.Text()), " ") == quote {
				found = true
			}
		})
		if !found {
			return SourceCheck{}, fmt.Errorf("source_check.empty_selector did not reproduce empty_text; inspect the actual response and correct the locator or extract available records")
		}
	}
	sum := sha256.Sum256([]byte(source.Body))
	return SourceCheck{Outcome: "no_match", URLs: []string{r.URL}, CheckedAt: r.FetchedAt, Meaning: "This checked response has an empty collection or displays the quoted empty state; it does not establish absence outside that source/query.", Evidence: []SourceCheckEvidence{{URL: r.URL, SourceDigest: hex.EncodeToString(sum[:]), Locator: locator}}}, nil
}
