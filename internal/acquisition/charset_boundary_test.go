package acquisition

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestPublicFetcherDecodesHTMLBeforeModelOrLuaReads(t *testing.T) {
	for _, label := range []string{"header", "meta", "utf8"} {
		t.Run(label, func(t *testing.T) {
			want := "预算五十三万元；截止日期十月二十日。"
			body := []byte("<html><main><p>" + want + "</p></main></html>")
			if label == "meta" {
				body = []byte("<html><meta charset=gb2312><main><p>" + want + "</p></main></html>")
			}
			if label != "utf8" {
				var err error
				body, err = simplifiedchinese.GBK.NewEncoder().Bytes(body)
				if err != nil {
					t.Fatal(err)
				}
			}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				contentType := "text/html"
				if label == "header" {
					contentType += "; charset=gbk"
				}
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write(body)
			}))
			defer remote.Close()
			f, err := NewPublicFetcher(InspectionPolicy([]string{"source.example"}))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			f.lookupIP = func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
			f.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(remote.URL, "http://"))
			}
			out, err := f.Fetch(context.Background(), "GET", "http://source.example/detail")
			if err != nil || !strings.Contains(out.Body, want) || out.Receipt.Bytes != int64(len(body)) {
				t.Fatal("raw charset bytes escaped host decoding or accounting changed", out, err)
			}
		})
	}
}
