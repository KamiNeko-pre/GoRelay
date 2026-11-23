package information

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

func executeAPI(ctx context.Context, plan APIPlan, fetch acquisition.FetchFunc) (engine.ProgramResult, error) {
	return executeAPIWithRequester(ctx, plan, func(ctx context.Context, opts acquisition.RequestOptions) (acquisition.Response, error) {
		return fetch(ctx, opts.Method, opts.URL)
	})
}

// A host-controlled CONNECT proxy maps a verified public fixture address to
// local test HTTP. Production still runs PublicFetcher and every scope check.
func publicSource(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	source := httptest.NewServer(handler)
	t.Cleanup(source.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "8.8.8.8:80" {
			http.Error(w, "unexpected proxy peer", 403)
			return
		}
		destination, err := net.Dial("tcp", strings.TrimPrefix(source.URL, "http://"))
		if err != nil {
			http.Error(w, "source unavailable", 502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			destination.Close()
			return
		}
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		go func() { io.Copy(destination, buffer); destination.Close() }()
		io.Copy(client, destination)
		client.Close()
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("GORELAY_ACQUISITION_PROXY", proxy.URL)
	return source
}
