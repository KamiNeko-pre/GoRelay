package information

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go-lua-crawler/internal/acquisition"
)

func (h *informationHost) inspectBrowserSource(ctx context.Context, target string) (any, error) {
	s := h.service
	if s.Browser == nil {
		return nil, actionFailure("BROWSER_UNAVAILABLE", fmt.Errorf("browser runtime is not configured"), "Use an accessible source or explain the source gap in partial delivery.")
	}
	if !urlInScope(target, h.input.SourceScope) {
		return nil, actionFailure("BROWSER_SOURCE_REFERENCE_INVALID", fmt.Errorf("browser fallback requires an explicit approved source URL"), "Use a URL returned in browser_approved_urls.")
	}
	step, fresh, err := s.reserveBrowserStep(ctx, h.request, "inspect_browser", target, map[string]any{"url": target, "scope": h.input.SourceScope})
	if err != nil {
		return nil, err
	}
	if !fresh {
		if step.ResultJSON == "" {
			return nil, ErrUnknownStep
		}
		return validRaw(step.ResultJSON), restoredDomainFailure(step)
	}
	fetcher, err := s.NewFetcher(h.input.SourceScope)
	if err != nil {
		return nil, actionFailure("ACQUISITION_FETCHER_UNAVAILABLE", err, "Check source transport configuration or continue with other available sources.")
	}
	defer fetcher.Close()
	runtime, cause := s.Browser.Render(ctx, BrowserPlan{URL: target}, h.input.SourceScope, fetcher)
	out := map[string]any{"url": target, "request_count": len(runtime.Requests)}
	if apis := compactBrowserNetworkAPIs(runtime.NetworkAPIs, target, 6000); len(apis) > 0 {
		out["network_apis"] = apis
	}
	if len(runtime.SourceEvidence) > 0 {
		capture := runtime.SourceEvidence[0]
		out["source"] = acquisitionResponseMetadata(capture)
		if reason, blocked := browserSourceDiagnosis(capture); cause == nil && blocked {
			cause = fmt.Errorf("%s", reason)
		} else if cause == nil {
			out["next_step"] = "Rendered HTML outline captured in source.html_outline, snapshot saved in references, and XHR/Fetch API calls summarized in network_apis. Replay public APIs or extract HTML via run_lua, or pass browser.fields to browser_extract."
		}
	} else if cause == nil {
		cause = fmt.Errorf("browser returned no captured response")
	}
	if cause != nil {
		cause = actionFailure("BROWSER_RENDER_FAILED", cause, "Read the returned render diagnostics. If the source still requires access or verification, use another source or report the gap.")
	}
	if err := h.commitAcquisition(ctx, step, nil, runtime.SourceEvidence, out, cause, nil); err != nil {
		return nil, err
	}
	return out, cause
}

func sameSiteOrSubdomain(targetHost, apiHost string) int {
	th := strings.ToLower(strings.TrimSuffix(targetHost, "."))
	ah := strings.ToLower(strings.TrimSuffix(apiHost, "."))
	if th == "" || ah == "" {
		return 0
	}
	if th == ah {
		return 2
	}
	if strings.HasSuffix(ah, "."+th) || strings.HasSuffix(th, "."+ah) {
		return 1
	}
	tp := strings.Split(th, ".")
	ap := strings.Split(ah, ".")
	if len(tp) >= 2 && len(ap) >= 2 && tp[len(tp)-1] == ap[len(ap)-1] && tp[len(tp)-2] == ap[len(ap)-2] {
		return 1
	}
	return 0
}

func schemaHasCollection(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var paths []jsonPathShape
	if json.Unmarshal(raw, &paths) != nil {
		return false
	}
	var walk func([]jsonPathShape) bool
	walk = func(list []jsonPathShape) bool {
		for _, p := range list {
			if p.Type == "array" && len(p.ItemPaths) > 0 {
				return true
			}
			if len(p.ItemPaths) > 0 && walk(p.ItemPaths) {
				return true
			}
		}
		return false
	}
	return walk(paths)
}

func compactAPISchema(raw json.RawMessage, maxBytes int) json.RawMessage {
	if len(raw) == 0 || maxBytes <= 0 || len(raw) <= maxBytes {
		return raw
	}
	var paths []jsonPathShape
	if json.Unmarshal(raw, &paths) != nil || len(paths) == 0 {
		return nil
	}
	// Prioritize array/collection descriptors and shallow top-level metadata keys
	// so one large config dictionary never starves other endpoints out of budget.
	ordered := make([]jsonPathShape, 0, len(paths))
	var shallow, arrays, rest []jsonPathShape
	for _, p := range paths {
		depth := strings.Count(p.Path, "/")
		if p.Type == "array" {
			trimmed := p
			if len(trimmed.ItemPaths) > 12 {
				trimmed.ItemPaths = trimmed.ItemPaths[:12]
			}
			arrays = append(arrays, trimmed)
		} else if depth <= 2 {
			trimmed := p
			if len(trimmed.ItemPaths) > 8 {
				trimmed.ItemPaths = trimmed.ItemPaths[:8]
			}
			shallow = append(shallow, trimmed)
		} else {
			rest = append(rest, p)
		}
	}
	ordered = append(ordered, arrays...)
	ordered = append(ordered, shallow...)
	ordered = append(ordered, rest...)
	var kept []jsonPathShape
	used := 2
	for _, p := range ordered {
		enc, err := json.Marshal(p)
		if err != nil {
			continue
		}
		if len(kept) > 0 && used+len(enc)+1 > maxBytes {
			// If an array entry had big ItemPaths, try trimming ItemPaths once more.
			if len(p.ItemPaths) > 4 {
				p.ItemPaths = p.ItemPaths[:4]
				if enc2, err2 := json.Marshal(p); err2 == nil && used+len(enc2)+1 <= maxBytes {
					used += len(enc2) + 1
					kept = append(kept, p)
				}
			}
			continue
		}
		used += len(enc) + 1
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return nil
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	return json.RawMessage(out)
}

func compactBrowserNetworkAPIs(apis []acquisition.NetworkAPICapture, targetRaw string, budgetBytes int) []acquisition.NetworkAPICapture {
	if len(apis) == 0 {
		return nil
	}
	targetHost := ""
	if tu, err := url.Parse(targetRaw); err == nil {
		targetHost = tu.Hostname()
	}
	type rankedAPI struct {
		cap       acquisition.NetworkAPICapture
		siteTier  int
		valueTier int
		ok2xx     bool
		hasJSON   bool
		order     int
	}
	byKey := map[string]rankedAPI{}
	for idx, api := range apis {
		u, err := url.Parse(api.URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		key := strings.ToUpper(api.Method) + " " + strings.ToLower(u.Scheme+"://"+u.Hostname()) + u.Path
		ok2xx := api.Status >= 200 && api.Status < 300
		hasJSON := len(api.ResponseSchema) > 0
		hasColl := schemaHasCollection(api.ResponseSchema)
		valueTier := 0
		switch {
		case api.SetsCookie || (strings.TrimSpace(api.RequestBody) != "" && ok2xx):
			valueTier = 3
		case hasColl && ok2xx:
			valueTier = 2
		case hasJSON && ok2xx:
			valueTier = 1
		}
		cand := rankedAPI{
			cap:       api,
			siteTier:  sameSiteOrSubdomain(targetHost, u.Hostname()),
			valueTier: valueTier,
			ok2xx:     ok2xx,
			hasJSON:   hasJSON,
			order:     idx,
		}
		if prev, exists := byKey[key]; exists {
			preferNew := (cand.valueTier > prev.valueTier) ||
				(cand.valueTier == prev.valueTier && !prev.ok2xx && cand.ok2xx) ||
				(cand.valueTier == prev.valueTier && prev.ok2xx == cand.ok2xx && len(cand.cap.ResponseSchema) > len(prev.cap.ResponseSchema)) ||
				(cand.valueTier == prev.valueTier && prev.ok2xx == cand.ok2xx && len(cand.cap.ResponseSchema) == len(prev.cap.ResponseSchema) && cand.order > prev.order)
			if !preferNew {
				continue
			}
		}
		byKey[key] = cand
	}
	ranked := make([]rankedAPI, 0, len(byKey))
	for _, r := range byKey {
		ranked = append(ranked, r)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].siteTier != ranked[j].siteTier {
			return ranked[i].siteTier > ranked[j].siteTier
		}
		if ranked[i].valueTier != ranked[j].valueTier {
			return ranked[i].valueTier > ranked[j].valueTier
		}
		if ranked[i].hasJSON != ranked[j].hasJSON {
			return ranked[i].hasJSON
		}
		if ranked[i].ok2xx != ranked[j].ok2xx {
			return ranked[i].ok2xx
		}
		return ranked[i].order < ranked[j].order
	})
	perAPISchemaBudget := 0
	if budgetBytes > 0 {
		slots := min(len(ranked), 4)
		if slots < 2 {
			slots = 2
		}
		perAPISchemaBudget = max(400, (budgetBytes/slots)-240)
	}
	var out []acquisition.NetworkAPICapture
	usedBytes := 2
	for _, r := range ranked {
		item := r.cap
		if perAPISchemaBudget > 0 && len(item.ResponseSchema) > perAPISchemaBudget {
			item.ResponseSchema = compactAPISchema(item.ResponseSchema, perAPISchemaBudget)
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		if len(out) > 0 && budgetBytes > 0 && usedBytes+len(encoded)+1 > budgetBytes {
			// Try omitting schema for lower-priority entries if the endpoint metadata itself fits.
			if len(item.ResponseSchema) > 0 {
				item.ResponseSchema = nil
				if encSlim, err2 := json.Marshal(item); err2 == nil && usedBytes+len(encSlim)+1 <= budgetBytes {
					usedBytes += len(encSlim) + 1
					out = append(out, item)
				}
			}
			continue
		}
		usedBytes += len(encoded) + 1
		out = append(out, item)
	}
	return out
}
