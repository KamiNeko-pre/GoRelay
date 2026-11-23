package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go-lua-crawler/internal/knowledge"
)

// This backend checks the MCP contract only; it is not a model or a successful
// acquisition fixture. Database and real-client acceptance are separate checks.
type protocolBackend struct{ passage string }

func (b protocolBackend) FindDeliveries(context.Context, DeliveryQuery) (DeliveryPage, error) {
	return DeliveryPage{Deliveries: []DeliveryEntry{}}, nil
}
func (b protocolBackend) ReadDelivery(context.Context, DeliveryID) (map[string]any, error) {
	return map[string]any{}, nil
}

func (b protocolBackend) FindSources(context.Context, SourceQuery) (SourcePage, error) {
	return SourcePage{Sources: []Source{}}, nil
}

func (b protocolBackend) SearchEvidence(ctx context.Context, in EvidenceInput) (EvidenceResult, error) {
	if in.Query == "storage-error" {
		return EvidenceResult{}, &ToolFailure{Code: "STORAGE_UNAVAILABLE", Message: "database connection refused", RecoverySteps: []string{"Restore the database connection, then retry the same query."}}
	}
	return EvidenceResult{Entries: []CitedEvidence{{EvidenceSnapshot: knowledge.EvidenceSnapshot{Evidence: knowledge.Evidence{ID: "snapshot_example", Text: b.passage}, Current: true, SourceActive: true}, SnapshotURL: RequestBaseURL(ctx) + "/evidence#id=snapshot_example"}}, Total: 1}, nil
}

func (b protocolBackend) ReadEvidence(ctx context.Context, in EvidenceIDs) (EvidenceResult, error) {
	out := EvidenceResult{Entries: []CitedEvidence{}}
	for _, id := range in.IDs {
		if id != "snapshot_example" {
			return out, &ToolFailure{Code: "EVIDENCE_NOT_FOUND", Message: "unknown evidence ID: " + id, RecoverySteps: []string{"Copy an evidence ID returned by gorelay_search_evidence."}}
		}
		out.Entries = append(out.Entries, CitedEvidence{EvidenceSnapshot: knowledge.EvidenceSnapshot{Evidence: knowledge.Evidence{ID: id, Text: b.passage}}, SnapshotURL: RequestBaseURL(ctx) + "/evidence#id=" + id})
	}
	out.Total = len(out.Entries)
	return out, nil
}

func (b protocolBackend) ReadData(context.Context, DataInput) (DataPage, error) {
	return DataPage{Records: []DataRecord{}}, nil
}

func (b protocolBackend) InspectSource(context.Context, InspectSourceInput) (SourceAction, error) {
	return SourceAction{}, nil
}
func (b protocolBackend) ReadObservation(context.Context, ObservationInput) (SourceAction, error) {
	return SourceAction{}, nil
}
func (b protocolBackend) ValidateProgram(context.Context, ValidateProgramInput) (SourceAction, error) {
	return SourceAction{}, nil
}
func (b protocolBackend) PublishSource(context.Context, PublishSourceInput) (SourceAction, error) {
	return SourceAction{}, nil
}

func (b protocolBackend) Collect(context.Context, CollectInput) (SourceJob, error) {
	return SourceJob{}, nil
}
func (b protocolBackend) GetJob(context.Context, JobInput) (SourceJob, error) {
	return SourceJob{}, nil
}
func (b protocolBackend) CancelJob(context.Context, JobInput) (SourceJob, error) {
	return SourceJob{}, nil
}

func TestHTTPToolsPreservePassagesAndActionableErrors(t *testing.T) {
	passage := strings.Repeat("可回查的原始片段", 1500)
	server := httptest.NewServer(NewHandler(protocolBackend{passage: passage}, ""))
	defer server.Close()
	ctx := t.Context()
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-check", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 13 {
		t.Fatalf("tool discovery: %v, %v", listed, err)
	}
	for _, tool := range listed.Tools {
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Fatalf("tool lacks input/output schema: %s", tool.Name)
		}
		if tool.Name == "gorelay_publish_source" && tool.Annotations.ReadOnlyHint {
			t.Fatal("publication advertised as read-only")
		}
	}
	for _, tool := range []string{"gorelay_read_observation", "gorelay_validate_program", "gorelay_publish_source"} {
		rejected, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
		if err != nil || !rejected.IsError {
			t.Fatalf("missing fields not returned for %s: %v %v", tool, rejected, err)
		}
		body := compatibleToolJSON(t, rejected)
		var reply ToolReply[SourceAction]
		if err := json.Unmarshal(body, &reply); err != nil || reply.Error == nil || len(reply.Error.RecoverySteps) == 0 || !strings.Contains(reply.Error.Message, "workspace_id") {
			t.Fatalf("missing-field recovery absent for %s: %s", tool, body)
		}
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "gorelay_search_evidence", Arguments: map[string]any{"query": "original fact"}})
	if err != nil || result.IsError {
		t.Fatalf("valid evidence query: %v, %v", result, err)
	}
	encoded := compatibleToolJSON(t, result)
	var reply ToolReply[EvidenceResult]
	if err := json.Unmarshal(encoded, &reply); err != nil || reply.Data == nil || len(reply.Data.Entries) != 1 || reply.Data.Entries[0].Text != passage || !strings.HasPrefix(reply.Data.Entries[0].SnapshotURL, server.URL) {
		t.Fatalf("passage or citation link lost: decode=%v", err)
	}
	for _, check := range []struct {
		args    map[string]any
		code    string
		message string
	}{
		{map[string]any{}, "INVALID_ARGUMENTS", "query"},
		{map[string]any{"query": "storage-error"}, "STORAGE_UNAVAILABLE", "connection refused"},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "gorelay_search_evidence", Arguments: check.args})
		if err != nil || !result.IsError {
			t.Fatalf("expected tool error: %v, %v", result, err)
		}
		encoded := compatibleToolJSON(t, result)
		var reply ToolReply[EvidenceResult]
		if err := json.Unmarshal(encoded, &reply); err != nil || reply.Error == nil || reply.Error.Code != check.code || !strings.Contains(reply.Error.Message, check.message) || len(reply.Error.RecoverySteps) == 0 {
			t.Fatalf("actionable diagnosis missing: %s, %v", encoded, err)
		}
	}
	for _, missing := range []string{"missing_snapshot_a", "missing_snapshot_b"} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "gorelay_read_evidence", Arguments: map[string]any{"evidence_ids": []string{"snapshot_example", missing}}})
		if err != nil || !result.IsError {
			t.Fatalf("partial evidence read must report its missing ID: %v %v", result, err)
		}
		encoded := compatibleToolJSON(t, result)
		var partial ToolReply[EvidenceResult]
		if err := json.Unmarshal(encoded, &partial); err != nil || partial.Data == nil || len(partial.Data.Entries) != 1 || partial.Data.Entries[0].Text != passage || partial.Error == nil || !strings.Contains(partial.Error.Message, missing) {
			t.Fatalf("completed evidence lost at MCP boundary: %s %v", encoded, err)
		}
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "gorelay_read_evidence", Arguments: map[string]any{"evidence_ids": []string{"snapshot_example"}}})
	if err != nil || result.IsError {
		t.Fatalf("valid evidence read changed: %v %v", result, err)
	}

}

func compatibleToolJSON(t *testing.T, result *mcp.CallToolResult) []byte {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil || body["schema_version"] != ContractVersion {
		t.Fatalf("versioned result missing: %s %v", encoded, err)
	}
	for _, content := range result.Content {
		text, ok := content.(*mcp.TextContent)
		if !ok {
			continue
		}
		var value any
		if json.Unmarshal([]byte(text.Text), &value) != nil {
			continue
		}
		fallback, _ := json.Marshal(value)
		if bytes.Equal(encoded, fallback) {
			return encoded
		}
	}
	t.Fatalf("text-only client would receive a different result: %v", result.Content)
	return nil
}
