package aiapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This opt-in protocol probe uses the real installed CLI against a local fake
// Responses endpoint. It spends no real model tokens and records the actual
// advertised tool list plus responses to an unsolicited native function call.
func TestInstalledCodexInferenceNativeBoundary(t *testing.T) {
	bin := os.Getenv("GORELAY_CODEX_BOUNDARY_BIN")
	if bin == "" {
		t.Skip("explicit installed CLI probe only")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "model.txt"), []byte(inferenceInstructions), 0600)
	os.WriteFile(filepath.Join(dir, "schema.json"), []byte(inferenceSchema), 0600)
	marker := filepath.Join(dir, "native-marker.txt")
	var mu sync.Mutex
	requests := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		n := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"type": "message", "id": "msg_probe", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": `{"content":"probe done","tool_calls":[]}`, "annotations": []any{}}}}
		if n == 1 {
			item = map[string]any{"type": "function_call", "id": "fc_probe", "call_id": "call_probe", "name": "exec_command", "arguments": fmt.Sprintf(`{"cmd":%s}`, strconv.Quote("echo should-not-run > "+marker)), "status": "completed"}
		}
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_probe", "object": "response", "status": "in_progress", "output": []any{}}},
			{"type": "response.output_item.added", "output_index": 0, "item": item},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": map[string]any{"id": "resp_probe", "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2, "input_tokens_details": map[string]any{"cached_tokens": 0}}}},
		} {
			raw, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
		}
	}))
	defer server.Close()
	args := codexInferenceArgs(dir, "gpt-6-luna", "max", envDefault("GORELAY_CODEX_BOUNDARY_WEB_SEARCH", "disabled"))
	if os.Getenv("GORELAY_CODEX_BOUNDARY_CODE_HOST") == "1" {
		filtered := []string{}
		for i := 0; i < len(args); i++ {
			if args[i] == "--disable" && i+1 < len(args) && (args[i+1] == "code_mode" || args[i+1] == "code_mode_only" || args[i+1] == "code_mode_host") {
				i++
				continue
			}
			filtered = append(filtered, args[i])
		}
		args = append(filtered, "--enable", "code_mode", "--enable", "code_mode_only", "--enable", "code_mode_host")
	}
	if catalog := os.Getenv("GORELAY_CODEX_BOUNDARY_CATALOG"); catalog != "" {
		args = append(args, "-c", "model_catalog_json="+strconv.Quote(filepath.ToSlash(catalog)))
	}
	args = append(args, "-c", "model_provider=boundary_fixture", "-c", "model_providers.boundary_fixture.name=boundary_fixture", "-c", "model_providers.boundary_fixture.base_url="+strconv.Quote(server.URL+"/v1"), "-c", "model_providers.boundary_fixture.wire_api=responses", "-c", "model_providers.boundary_fixture.requires_openai_auth=false")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if err := os.MkdirAll(filepath.Join(dir, "private-home"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd.Env = inferenceHomeEnvironment(os.Environ(), filepath.Join(dir, "private-home"))
	cmd.Stdin = strings.NewReader(`{"messages":[{"role":"user","content":"local boundary probe"}]}`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	mu.Lock()
	defer mu.Unlock()
	// Preserve protocol evidence even when a requested configuration violates
	// the tool-free boundary; this is a local diagnosis, never an API success.
	if out := os.Getenv("GORELAY_CODEX_BOUNDARY_EVIDENCE"); out != "" {
		data, _ := json.MarshalIndent(map[string]any{"backend": "local_fake_responses", "real_model_tokens": 0, "requests": requests, "events": stdout.String(), "stderr": stderr.String()}, "", "  ")
		if err := os.WriteFile(out, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if len(requests) == 0 {
		t.Fatalf("CLI never reached local protocol probe: %v %s", err, stderr.String())
	}
	for _, request := range requests {
		if tools, ok := request["tools"].([]any); ok && len(tools) > 0 {
			t.Fatalf("native tools remain advertised: %+v", tools)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unsolicited command executed")
	}
	if len(requests) < 2 {
		t.Fatalf("native call rejection not observed: %v %s %s", err, stdout.String(), stderr.String())
	}
	raw, _ := json.Marshal(requests[1]["input"])
	if !strings.Contains(string(raw), "Unknown tool") && !strings.Contains(string(raw), "unsupported") && !strings.Contains(string(raw), "not found") {
		t.Fatalf("missing native rejection receipt: %s", raw)
	}
	if out := os.Getenv("GORELAY_CODEX_BOUNDARY_EVIDENCE"); out != "" {
		data, _ := json.MarshalIndent(map[string]any{"backend": "local_fake_responses", "real_model_tokens": 0, "requests": requests, "events": stdout.String(), "stderr": stderr.String(), "native_marker_absent": true}, "", "  ")
		if err := os.WriteFile(out, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
