package acquisition

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// A trusted host-level HTTP CONNECT proxy routes only verified public IPs.
// Transport.Proxy stays nil: letting a proxy resolve an unverified hostname
// would bypass DNS pinning and permit access to the proxy's private network.
func (f *PublicFetcher) configureProxy() error {
	raw := strings.TrimSpace(os.Getenv("GORELAY_ACQUISITION_PROXY"))
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("GORELAY_ACQUISITION_PROXY must be an HTTP CONNECT proxy URL")
	}
	f.proxyURL = u
	return nil
}

func (f *PublicFetcher) dialPublicPeer(ctx context.Context, network, verifiedAddress string) (net.Conn, error) {
	if f.proxyURL == nil {
		return f.dial(ctx, network, verifiedAddress)
	}
	proxyAddress := f.proxyURL.Host
	if f.proxyURL.Port() == "" {
		proxyAddress = net.JoinHostPort(f.proxyURL.Hostname(), "80")
	}
	conn, err := f.dial(ctx, network, proxyAddress)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(f.policy.Timeout)
	if end, exists := ctx.Deadline(); exists && end.Before(deadline) {
		deadline = end
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: verifiedAddress}, Host: verifiedAddress, Header: make(http.Header)}
	if f.proxyURL.User != nil {
		password, _ := f.proxyURL.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(f.proxyURL.User.Username()+":"+password)))
	}
	if err = request.Write(conn); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("outbound proxy CONNECT status %d", response.StatusCode)
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ok = true
	return &bufferedTunnel{Conn: conn, reader: reader}, nil
}

type bufferedTunnel struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedTunnel) Read(p []byte) (int, error) { return c.reader.Read(p) }
