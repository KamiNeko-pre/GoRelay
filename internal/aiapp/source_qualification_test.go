package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Optional stage-0 replay of privately saved sources, never a live Agent score.
// Uses the real inspection and model projection functions without HTTP/model calls.
func TestSavedBusinessSourcesInspectionProjection(t *testing.T) {
	root := os.Getenv("GORELAY_SOURCE_QUALIFICATION_DIR")
	if root == "" {
		t.Skip("saved source qualification directory not configured")
	}
	type probe struct {
		Input struct {
			ID string `json:"id"`
		} `json:"input"`
		Receipt acquisition.Receipt `json:"receipt"`
		Stats   acquisition.Stats   `json:"stats"`
		RawFile string              `json:"raw_file"`
		Error   string              `json:"error"`
	}
	type measurement struct {
		ID                       string `json:"id"`
		SourceFile               string `json:"source_file"`
		ResponseBytes            int    `json:"response_bytes"`
		PersistedInspectionBytes int    `json:"persisted_inspection_bytes"`
		ModelObservationBytes    int    `json:"model_observation_bytes"`
		DOMBlocks                int    `json:"dom_blocks"`
		ReceiptPaged             bool   `json:"receipt_paged"`
		SourcePreviewTruncated   bool   `json:"source_preview_truncated"`
	}
	var measurements []measurement
	for _, directory := range []string{"source-identity", "sources-main", "source-linked"} {
		data, err := os.ReadFile(filepath.Join(root, directory, "probe.json"))
		if err != nil {
			t.Fatal(err)
		}
		var probes []probe
		if err := json.Unmarshal(data, &probes); err != nil {
			t.Fatal(err)
		}
		for _, p := range probes {
			if p.Error != "" || p.RawFile == "" {
				continue
			}
			if !strings.Contains(p.Input.ID, "pricing") && !strings.Contains(p.Input.ID, "billing") {
				continue
			}
			t.Run(p.Input.ID, func(t *testing.T) {
				body, err := os.ReadFile(filepath.Join(root, directory, p.RawFile))
				if err != nil {
					t.Fatal(err)
				}
				if len(body) != int(p.Receipt.Bytes) {
					t.Fatalf("saved body/receipt size mismatch: %d/%d", len(body), p.Receipt.Bytes)
				}
				inspection := inspectResponse(acquisition.Response{Body: string(body), Receipt: p.Receipt}, p.Stats)
				raw, err := json.Marshal(inspection)
				if err != nil {
					t.Fatal(err)
				}
				before := string(raw)
				view := modelObservation(ToolResult{CallID: "qualification-" + p.Input.ID, Name: "inspect_source", Data: raw})
				if len(view.Data) > observationBytes {
					t.Fatalf("model observation exceeds %d bytes: %d", observationBytes, len(view.Data))
				}
				if !json.Valid(view.Data) || string(raw) != before {
					t.Fatal("projection invalid or changed original receipt")
				}
				var original sourceInspection
				if err := json.Unmarshal(raw, &original); err != nil || original.Source.Body != string(body) {
					t.Fatal("canonical source lost")
				}
				var projected struct {
					ReceiptView       string `json:"_receipt_view"`
					PreviewTruncated  bool   `json:"preview_truncated"`
					ReceiptToolCallID string `json:"receipt_tool_call_id"`
					Observation       struct {
						PreviewTruncated bool `json:"preview_truncated"`
					} `json:"observation"`
				}
				if err := json.Unmarshal(view.Data, &projected); err != nil {
					t.Fatal(err)
				}
				if len([]rune(string(body))) > 2000 && !projected.PreviewTruncated && projected.ReceiptToolCallID != "qualification-"+p.Input.ID {
					t.Fatal("truncated source has no preview/page continuation")
				}
				m := measurement{ID: p.Input.ID, SourceFile: filepath.ToSlash(filepath.Join(directory, p.RawFile)), ResponseBytes: len(body), PersistedInspectionBytes: len(raw), ModelObservationBytes: len(view.Data), DOMBlocks: len(inspection.Structure), ReceiptPaged: projected.ReceiptView != "", SourcePreviewTruncated: projected.PreviewTruncated || projected.Observation.PreviewTruncated}
				measurements = append(measurements, m)
				if err := os.MkdirAll(filepath.Join(root, "model-observations"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "model-observations", p.Input.ID+".json"), view.Data, 0644); err != nil {
					t.Fatal(err)
				}
				t.Logf("response=%d persisted=%d observation=%d blocks=%d receipt_paged=%t", m.ResponseBytes, m.PersistedInspectionBytes, m.ModelObservationBytes, m.DOMBlocks, m.ReceiptPaged)
			})
		}
	}
	if len(measurements) < 6 {
		t.Fatal("qualification is missing business source fixtures")
	}
	data, err := json.MarshalIndent(measurements, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "model-observation-measurements.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}
