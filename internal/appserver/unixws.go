package appserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/coder/websocket"
)

// ParseUnixEndpoint validates the explicit unix:///absolute/path form used by
// the managed adapter and returns its decoded filesystem path.
func ParseUnixEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("appserver: parse endpoint: %w", err)
	}
	if u.Scheme != "unix" {
		return "", fmt.Errorf("appserver: endpoint scheme must be unix, got %q", u.Scheme)
	}
	if u.Host != "" {
		return "", fmt.Errorf("appserver: unix endpoint must not contain a host: %q", u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("appserver: unix endpoint must not contain a query or fragment")
	}
	path, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		return "", fmt.Errorf("appserver: decode unix socket path: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("appserver: unix socket path must be absolute: %q", path)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("appserver: unix socket path contains NUL")
	}
	return filepath.Clean(path), nil
}

// DialUnix performs a standard websocket HTTP Upgrade over the endpoint's
// Unix-domain socket. The synthetic ws://localhost/ URL supplies only the HTTP
// request target and Host header; the transport cannot fall back to TCP.
func DialUnix(ctx context.Context, endpoint string, opts Options) (*Client, error) {
	path, err := ParseUnixEndpoint(endpoint)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		},
		ForceAttemptHTTP2: false,
	}
	// A rejected upgrade is an ordinary HTTP response. websocket.Dial drains
	// that response, which makes its Unix connection eligible for this private
	// transport's idle pool. The transport is otherwise unreachable after this
	// function returns, so close its pool on both success and failure. A
	// successful 101 connection has already been detached from the transport.
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	conn, response, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient:      httpClient,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
			return nil, fmt.Errorf("appserver: websocket upgrade returned HTTP %s: %w", response.Status, err)
		}
		return nil, fmt.Errorf("appserver: dial unix socket %s: %w", path, err)
	}
	return newClient(conn, opts), nil
}
