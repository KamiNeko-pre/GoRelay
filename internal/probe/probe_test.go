package probe

import (
	"context"
	"strings"
	"testing"

	"gorelay/internal/acquisition"
	"gorelay/internal/fetchgov"
)

type probeRequesterStub struct {
	body        string
	contentType string
}

func (s *probeRequesterStub) FetchRequest(_ context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
	if strings.HasSuffix(opts.URL, "/robots.txt") {
		return acquisition.Response{Receipt: acquisition.Receipt{Status: 404}}, nil
	}
	return acquisition.Response{
		Body: s.body,
		Receipt: acquisition.Receipt{
			URL:     opts.URL,
			Status:  200,
			Headers: map[string]string{"content-type": s.contentType},
		},
	}, nil
}
func (s *probeRequesterStub) Close() {}

type probeRendererStub struct {
	res fetchgov.RenderResult
}

func (r *probeRendererStub) RenderAndSniff(context.Context, fetchgov.RenderRequest) (fetchgov.RenderResult, error) {
	return r.res, nil
}

func TestProberDetectsDirectJSONAPIAndPagination(t *testing.T) {
	cfg := fetchgov.DefaultConfig()
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &probeRequesterStub{
			contentType: "application/json",
			body: `{
				"next_cursor": "cur_2",
				"data": {
					"releases": [
						{"id": "v1.26.0", "name": "Go 1.26.0", "prerelease": false, "downloads": 42}
					]
				}
			}`,
		}, nil
	}
	client, err := fetchgov.NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := NewProber(client).Probe(context.Background(), ProbeRequest{URL: "https://api.example.com/releases", DetectAPI: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != "json_api" || out.RecommendedExecutor != "http_api" || out.JSONSummary == nil {
		t.Fatalf("unexpected probe outcome: %+v", out)
	}
	if out.JSONSummary.ItemsPath != "/data/releases" || out.JSONSummary.PaginationHint != "next_cursor" {
		t.Fatalf("unexpected items path or pagination: %+v", out.JSONSummary)
	}
	if out.DraftAPISpec == nil || out.DraftAPISpec.ItemsPath != "/data/releases" {
		t.Fatalf("missing draft API spec: %+v", out.DraftAPISpec)
	}
}

func TestProberDetectsEmbeddedAndXHRCandidatesOnSPA(t *testing.T) {
	cfg := fetchgov.DefaultConfig()
	cfg.RequestsPerSec = 100
	cfg.Burst = 100
	cfg.RequesterFactory = func(acquisition.Policy) (fetchgov.Requester, error) {
		return &probeRequesterStub{
			contentType: "text/html",
			body:        `<html><body><div id="root"></div><noscript>Enable JavaScript</noscript></body></html>`,
		}, nil
	}
	cfg.Renderer = &probeRendererStub{
		res: fetchgov.RenderResult{
			FinalURL: "https://app.example.com",
			HTML:     `<html><body><article><h3>Item 1</h3><a href="/1">read</a></article><article><h3>Item 2</h3><a href="/2">read</a></article></body></html>`,
			NetworkAPIs: []acquisition.NetworkAPICapture{{
				Method:         "GET",
				URL:            "https://app.example.com/api/v1/feed?page=1",
				Status:         200,
				ContentType:    "application/json",
				ResponseSchema: map[string]string{"title": "string", "id": "string"},
			}},
		},
	}
	client, err := fetchgov.NewSafeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := NewProber(client).Probe(context.Background(), ProbeRequest{URL: "https://app.example.com", DetectAPI: true, UseBrowser: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RecommendedExecutor != "http_api" || len(out.NetworkCaptures) != 1 || out.DraftAPISpec.URLTemplate != "https://app.example.com/api/v1/feed?page=1" {
		t.Fatalf("expected XHR API sniff upgrade to http_api: %+v", out)
	}
	if len(out.DOMSelectors) == 0 {
		t.Fatal("expected rendered DOM selectors summary")
	}
	if len(out.LuaSandboxContract) > 1200 {
		t.Fatalf("lua contract unexpectedly bloated: %d chars", len(out.LuaSandboxContract))
	}
}
