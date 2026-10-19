// ai-check exercises individual adapters without pretending the learner-owned
// Agent decision loop is complete. Default status mode makes no network calls.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
)

func configurationStatus() string {
	configured := func(names ...string) bool {
		for _, name := range names {
			if os.Getenv(name) == "" {
				return false
			}
		}
		return true
	}
	data, _ := json.Marshal(map[string]bool{"chat_configured": configured("GORELAY_AI_BASE_URL", "GORELAY_AI_API_KEY", "GORELAY_AI_MODEL"), "embedding_configured": configured("GORELAY_EMBED_BASE_URL", "GORELAY_EMBED_API_KEY", "GORELAY_EMBED_MODEL"), "search_configured": configured("GORELAY_SEARCH_API_KEY"), "runner_configured": configured("GORELAY_RUNNER_URL", "GORELAY_RUNNER_TOKEN"), "live_verified": false})
	return string(data)
}
func readInput(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("-input file is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("input exceeds 1 MiB")
	}
	return data, nil
}
func model(prefix string) (*aiapp.ModelProvider, error) {
	return aiapp.NewModelProvider(aiapp.ProviderConfig{BaseURL: os.Getenv(prefix + "BASE_URL"), APIKey: os.Getenv(prefix + "API_KEY"), Model: os.Getenv(prefix + "MODEL")})
}

type replayInput struct {
	Program   engine.ProgramRequest     `json:"program"`
	Responses []acquisition.ReplayEntry `json:"responses"`
}

func main() {
	mode := flag.String("mode", "status", "status | chat | embed | search | execute | replay | contract")
	input := flag.String("input", "", "UTF-8 prompt or JSON input file")
	query := flag.String("query", "", "search query")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	var out any
	var err error
	switch *mode {
	case "status":
		fmt.Println(configurationStatus())
		return
	case "search":
		var p *aiapp.BraveSearch
		p, err = aiapp.NewBraveSearch(aiapp.SearchConfig{Endpoint: os.Getenv("GORELAY_SEARCH_ENDPOINT"), APIKey: os.Getenv("GORELAY_SEARCH_API_KEY")})
		if err == nil {
			out, err = p.Search(ctx, *query, 5)
		}
	case "chat", "embed":
		var data []byte
		data, err = readInput(*input)
		if err != nil {
			break
		}
		prefix := "GORELAY_AI_"
		if *mode == "embed" {
			prefix = "GORELAY_EMBED_"
		}
		var p *aiapp.ModelProvider
		p, err = model(prefix)
		if err != nil {
			break
		}
		if *mode == "embed" {
			out, err = p.Embed(ctx, []string{string(data)})
		} else {
			out, err = p.Chat(ctx, aiapp.ChatRequest{Messages: []aiapp.Message{{Role: "user", Content: string(data)}}, MaxCompletionTokens: 1024})
		}
	case "execute":
		var data []byte
		data, err = readInput(*input)
		if err != nil {
			break
		}
		var in aiapp.RunnerRequest
		err = json.Unmarshal(data, &in)
		if err != nil {
			break
		}
		var p *aiapp.RunnerClient
		p, err = aiapp.NewRunnerClient(os.Getenv("GORELAY_RUNNER_URL"), os.Getenv("GORELAY_RUNNER_TOKEN"))
		if err == nil {
			out, err = p.Execute(ctx, in)
		}
	case "replay":
		var data []byte
		data, err = readInput(*input)
		if err != nil {
			break
		}
		var in replayInput
		err = json.Unmarshal(data, &in)
		if err != nil {
			break
		}
		replay := acquisition.NewReplay(in.Responses)
		var result engine.ProgramResult
		result, err = engine.RunProgram(ctx, in.Program, engine.DefaultProgramLimits(), replay.Fetch)
		out = struct {
			Mode                string               `json:"mode"`
			Result              engine.ProgramResult `json:"result"`
			UnconsumedResponses int                  `json:"unconsumed_responses"`
		}{"offline_replay", result, replay.Remaining()}
	case "contract":
		var data []byte
		data, err = readInput(*input)
		if err != nil {
			break
		}
		var spec aiapp.TaskSpec
		err = json.Unmarshal(data, &spec)
		if err == nil {
			err = spec.Validate()
		}
		out = spec
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
