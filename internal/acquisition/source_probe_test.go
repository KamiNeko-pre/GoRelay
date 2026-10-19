package acquisition

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// M0 availability probe, not an Agent success or input hint. Saved responses
// are private evaluation material and never put in a business Run's context.
func TestProcurementSourceProbe(t *testing.T) {
	out := os.Getenv("GORELAY_SOURCE_PROBE_DIR")
	if out == "" {
		t.Skip("explicit M0 source probe only")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	results := []map[string]any{}
	for _, source := range []struct{ id, domain, url string }{
		{"A-list", "bidding.swjtu.edu.cn", "https://bidding.swjtu.edu.cn/cggg.htm"},
		{"A-historical-security", "bidding.swjtu.edu.cn", "https://bidding.swjtu.edu.cn/info/1017/74087.htm"},
		{"A-historical-negative", "bidding.swjtu.edu.cn", "https://bidding.swjtu.edu.cn/info/1016/67057.htm"},
		{"B-list", "cg.uestc.edu.cn", "https://cg.uestc.edu.cn/f/list-16b1c59063ab42bd84c0ea4c56cfa16e.html"},
	} {
		f, err := NewPublicFetcher(InspectionPolicy([]string{source.domain}))
		if err != nil {
			t.Fatal(err)
		}
		response, err := f.Fetch(context.Background(), http.MethodGet, source.url)
		entry := map[string]any{"id": source.id, "url": source.url, "stats": f.Stats(), "receipt": response.Receipt, "available": err == nil}
		f.Close()
		if err != nil {
			entry["error"] = err.Error()
		} else {
			if err := os.WriteFile(filepath.Join(out, source.id+".html"), []byte(response.Body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		results = append(results, entry)
		t.Logf("%s available=%v requests=%v bytes=%v error=%v", source.id, err == nil, entry["stats"].(Stats).Requests, len(response.Body), err)
	}
	raw, _ := json.MarshalIndent(map[string]any{"purpose": "M0-engineering-probe-not-agent-run", "results": results}, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "probe.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
