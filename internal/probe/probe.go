package probe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gorelay/internal/fetchgov"
)

// Compact Lua runtime contract (< 350 tokens) returned by probe_source so host
// agents write valid gopher-lua connectors on the first pass.
const CompactLuaContract = `Sandbox globals: TARGET_URL (string), PARAMS (table).
Functions:
- http_request({url=..., method="GET", headers={...}, body="..."}) -> {ok, status, final_url, body, headers}
- json_decode(str) -> table | html_find(html, css) -> text | html_find_all(html, css) -> {texts}
- html_attr_all(html, css, attr) -> {values} | clean_text(str) -> str | url_resolve(base, rel) -> url
- checkpoint.get() -> cursor_table|nil
- checkpoint.save(cursor_table|nil, records_array) -- atomic batch upsert + CAS cursor advance (<=200 items/call)
Return: return true, {items=records}`

type Prober struct {
	client *fetchgov.SafeClient
}

func NewProber(client *fetchgov.SafeClient) *Prober {
	return &Prober{client: client}
}

func (p *Prober) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	if req.UseBrowser == "" {
		req.UseBrowser = "auto"
	}
	res := ProbeResult{
		URL:                req.URL,
		RobotsAllowed:      true,
		LuaSandboxContract: CompactLuaContract,
	}

	resp, err := p.client.Fetch(ctx, fetchgov.FetchRequest{
		URL:         req.URL,
		Method:      http.MethodGet,
		AuthProfile: req.AuthProfile,
	})
	if err != nil {
		var ge *fetchgov.GovernanceError
		if errors.As(err, &ge) && ge.Code == fetchgov.ErrCodeRobotsDisallowed {
			res.RobotsAllowed = false
			res.Kind = "blocked"
			return res, err
		}
		if errors.As(err, &ge) {
			res.Kind = "blocked"
			res.Status = ge.StatusCode
		}
		return res, err
	}

	res.FinalURL = resp.Receipt.URL
	if res.FinalURL == "" {
		res.FinalURL = req.URL
	}
	res.Status = resp.Receipt.Status
	res.ContentType = resp.Receipt.Headers["content-type"]

	lowerCT := strings.ToLower(res.ContentType)
	trimmed := strings.TrimSpace(resp.Body)
	if strings.Contains(lowerCT, "json") || strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if sum, jerr := SummarizeJSON([]byte(trimmed)); jerr == nil {
			res.Kind = "json_api"
			res.JSONSummary = sum
			res.RecommendedExecutor = "http_api"
			res.CandidateAPIs = []CandidateAPI{{
				Source:         "direct_endpoint",
				Method:         http.MethodGet,
				URL:            res.FinalURL,
				Status:         res.Status,
				ContentType:    res.ContentType,
				ItemsPath:      sum.ItemsPath,
				PaginationHint: sum.PaginationHint,
				SampleFields:   sum.Fields,
				SampleRow:      sum.SampleRow,
			}}
			res.DraftAPISpec = buildDraftSpec(res.FinalURL, sum)
			return res, nil
		}
	}

	if strings.Contains(lowerCT, "rss+xml") || strings.Contains(lowerCT, "atom+xml") || strings.Contains(trimmed, "<rss") || strings.Contains(trimmed, "<feed") {
		res.Kind = "feed"
		res.RecommendedExecutor = "lua_sandbox"
		return res, nil
	}

	embedded := ExtractEmbeddedJSON(resp.Body, res.FinalURL)
	if len(embedded) > 0 {
		res.CandidateAPIs = append(res.CandidateAPIs, embedded...)
	}
	selectors, spaShell := ExtractDOMSummary(resp.Body)
	res.DOMSelectors = selectors

	shouldBrowser := req.UseBrowser == "always" || (req.UseBrowser == "auto" && spaShell)
	if shouldBrowser {
		renderRes, rerr := p.client.Render(ctx, fetchgov.RenderRequest{URL: res.FinalURL})
		if rerr == nil {
			if renderRes.HTML != "" {
				renderedSelectors, _ := ExtractDOMSummary(renderRes.HTML)
				if len(renderedSelectors) > 0 {
					res.DOMSelectors = renderedSelectors
				}
			}
			for _, capAPI := range renderRes.NetworkAPIs {
				res.NetworkCaptures = append(res.NetworkCaptures, capAPI)
				var fields map[string]string
				if len(capAPI.ResponseSchema) > 0 {
					_ = json.Unmarshal(capAPI.ResponseSchema, &fields)
				}
				res.CandidateAPIs = append(res.CandidateAPIs, CandidateAPI{
					Source:       "xhr_network",
					Method:       capAPI.Method,
					URL:          capAPI.URL,
					Status:       capAPI.Status,
					ContentType:  capAPI.ContentType,
					SampleFields: fields,
				})
			}
		} else {
			res.Warnings = append(res.Warnings, "SPA shell detected but sidecar renderer unavailable: "+rerr.Error())
		}
	}

	if len(res.NetworkCaptures) > 0 {
		res.Kind = "json_api"
		res.RecommendedExecutor = "http_api"
		res.DraftAPISpec = &DraftAPISpec{
			URLTemplate: res.NetworkCaptures[0].URL,
			Method:      res.NetworkCaptures[0].Method,
		}
	} else if spaShell {
		res.Kind = "html_spa_candidate"
		res.RecommendedExecutor = "lua_sandbox"
	} else {
		res.Kind = "html_static"
		res.RecommendedExecutor = "lua_sandbox"
	}
	return res, nil
}

func buildDraftSpec(endpoint string, sum *JSONSummary) *DraftAPISpec {
	if sum == nil {
		return nil
	}
	mapping := make(map[string]string, len(sum.Fields))
	for field := range sum.Fields {
		mapping[field] = "/" + escapePointer(field)
	}
	pag := "none"
	if sum.PaginationHint != "" {
		pag = sum.PaginationHint
	}
	return &DraftAPISpec{
		URLTemplate:  endpoint,
		Method:       http.MethodGet,
		ItemsPath:    sum.ItemsPath,
		FieldMapping: mapping,
		Pagination:   pag,
	}
}
