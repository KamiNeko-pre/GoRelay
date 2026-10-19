// knowledge-import prepares a validated full-text document for the knowledge
// pipeline. Persistence and vector indexing are separate steps in Day 2.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/ruleconfig"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	inputPath := flag.String("input", "", "UTF-8 Markdown, plain text, or saved HTML file")
	format := flag.String("format", "markdown", "markdown, text, or html")
	source := flag.String("source", "manual", "source name")
	sourceKey := flag.String("source-key", "", "stable identity within this source")
	url := flag.String("url", "", "optional original HTTP(S) URL")
	title := flag.String("title", "", "title; Markdown can use its first level-one heading")
	configPath := flag.String("extract-config", "", "JSON selectors required for HTML")
	previewInput := flag.Bool("preview-input", false, "validate and show normalized input before document construction")
	flag.Parse()
	if *inputPath == "" {
		return fmt.Errorf("-input is required")
	}
	limit := knowledge.MaxBodyBytes
	if *format == "html" {
		limit = 4 << 20
	}
	content, err := readBounded(*inputPath, limit)
	if err != nil {
		return err
	}
	input := knowledge.Input{Source: *source, SourceKey: *sourceKey, URL: *url, Title: *title, Body: string(content), Format: *format}
	if *format == "html" {
		if *configPath == "" {
			return fmt.Errorf("-extract-config is required for HTML")
		}
		configFile, err := os.Open(*configPath)
		if err != nil {
			return fmt.Errorf("open extraction config: %w", err)
		}
		defer configFile.Close()
		decoder := json.NewDecoder(io.LimitReader(configFile, 64<<10))
		decoder.DisallowUnknownFields()
		var extraction ruleconfig.Config
		if err := decoder.Decode(&extraction); err != nil {
			return fmt.Errorf("decode extraction config: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("extraction config must contain exactly one JSON object")
		}
		article, err := ruleconfig.Extract(string(content), extraction)
		if err != nil {
			return err
		}
		input.Title, input.Body, input.Format = article.Title, article.Body, "text"
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if *previewInput {
		prepared, err := knowledge.NormalizeInput(input)
		if err != nil {
			return err
		}
		return encoder.Encode(struct {
			Stage string          `json:"stage"`
			Input knowledge.Input `json:"input"`
		}{Stage: "normalized_input", Input: prepared})
	}
	document, err := knowledge.NewDocument(input)
	if err != nil {
		return err
	}
	return encoder.Encode(document)
}

func readBounded(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open input: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	if len(content) > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	return content, nil
}
