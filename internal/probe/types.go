package probe

import "gorelay/internal/acquisition"

type ProbeRequest struct {
	URL         string `json:"url"`
	DetectAPI   bool   `json:"detect_api,omitempty"`
	UseBrowser  string `json:"use_browser,omitempty"` // "auto" (default), "always", "never"
	AuthProfile string `json:"auth_profile,omitempty"`
}

type CandidateAPI struct {
	Source         string            `json:"source"` // "direct_endpoint" | "embedded_json" | "xhr_network" | "rss_feed"
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	Status         int               `json:"status,omitempty"`
	ContentType    string            `json:"content_type,omitempty"`
	ItemsPath      string            `json:"items_path,omitempty"`
	PaginationHint string            `json:"pagination_hint,omitempty"`
	SampleFields   map[string]string `json:"sample_fields,omitempty"`
	SampleRow      map[string]any    `json:"sample_row,omitempty"`
}

type JSONSummary struct {
	RootType       string            `json:"root_type"`
	ItemsPath      string            `json:"items_path,omitempty"`
	ItemCount      int               `json:"item_count,omitempty"`
	PaginationHint string            `json:"pagination_hint,omitempty"`
	Fields         map[string]string `json:"fields,omitempty"`
	SampleRow      map[string]any    `json:"sample_row,omitempty"`
}

type DOMSelectorSample struct {
	Selector    string   `json:"selector"`
	Count       int      `json:"count"`
	SampleTexts []string `json:"sample_texts,omitempty"`
	SampleLinks []string `json:"sample_links,omitempty"`
}

type DraftAPISpec struct {
	URLTemplate  string            `json:"url_template"`
	Method       string            `json:"method"`
	ItemsPath    string            `json:"items_path,omitempty"`
	FieldMapping map[string]string `json:"field_mapping,omitempty"`
	Pagination   string            `json:"pagination,omitempty"`
}

type ProbeResult struct {
	URL                 string                          `json:"url"`
	FinalURL            string                          `json:"final_url,omitempty"`
	Status              int                             `json:"status,omitempty"`
	ContentType         string                          `json:"content_type,omitempty"`
	Kind                string                          `json:"kind"` // "json_api" | "feed" | "html_static" | "html_spa_candidate" | "blocked"
	RobotsAllowed       bool                            `json:"robots_allowed"`
	CandidateAPIs       []CandidateAPI                  `json:"candidate_apis,omitempty"`
	JSONSummary         *JSONSummary                    `json:"json_summary,omitempty"`
	DOMSelectors        []DOMSelectorSample             `json:"dom_selectors,omitempty"`
	RecommendedExecutor string                          `json:"recommended_executor,omitempty"` // "http_api" | "lua_sandbox"
	DraftAPISpec        *DraftAPISpec                   `json:"draft_api_spec,omitempty"`
	LuaSandboxContract  string                          `json:"lua_sandbox_contract,omitempty"`
	NetworkCaptures     []acquisition.NetworkAPICapture `json:"network_captures,omitempty"`
	Warnings            []string                        `json:"warnings,omitempty"`
}
