package information

import (
	"encoding/json"
	"testing"
)

func TestNoMatchProtocolIsReachableOnBrowserAndCrawler(t *testing.T) {
	for _, d := range informationToolDefinitions() {
		if d.Function.Name != "browser_extract" {
			continue
		}
		var shape struct {
			Properties map[string]struct{ Properties map[string]json.RawMessage }
		}
		if err := json.Unmarshal(d.Function.Parameters, &shape); err != nil {
			t.Fatal(err)
		}
		if shape.Properties["browser"].Properties["empty_state"] == nil {
			t.Error("browser cannot declare an observed empty-state marker separately from a wrong row selector")
		}
	}
}
