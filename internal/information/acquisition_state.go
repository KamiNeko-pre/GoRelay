package information

import "go-lua-crawler/internal/aiapp"

type assetInvocation struct {
	AssetID string         `json:"asset_id,omitempty"`
	URL     string         `json:"url,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
}
type sourceJobResult struct {
	Records []aiapp.Record `json:"records"`
	Asset   *Asset         `json:"asset,omitempty"`
}
