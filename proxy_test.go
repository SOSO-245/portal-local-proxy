package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every network listener in this suite is local. Origin names are sent to a
// simulated, explicitly trusted upstream; direct DNS tests inject a resolver.
type testLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *testLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *testLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func testProxyConfig(upstream string) Config {
	cfg := DefaultConfig()
	cfg.Upstream = upstream
	cfg.DialTimeoutSeconds = 2
	cfg.IdleTimeoutSeconds = 5
	cfg.MaxConnections = 8
	return cfg
}

func startTestProxy(t *testing.T, cfg Config) (*Proxy, *httptest.Server, *testLogBuffer) {
	t.Helper()
	logs := &testLogBuffer{}
	p, err := NewProxy(cfg, log.New(logs, "", 0))
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	s := httptest.NewServer(p)
	t.Cleanup(func() { p.Close(); s.Close() })
	return p, s, logs
}

func testHTTPClient(t *testing.T, proxyURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(u), DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 4 * time.Second}
}

func readTestBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return body
}

func waitTestLog(t *testing.T, logs *testLogBuffer, marker string) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if text := logs.String(); strings.Contains(text, marker) {
			return text
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("log did not contain %q: %s", marker, logs.String())
	return ""
}

func TestForwardPreservesPayloadAndEndToEndHeaders(t *testing.T) {
	type observed struct {
		method, target, host, body string
		header                     http.Header
	}
	seen := make(chan observed, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "request body read failed", 500)
			return
		}
		seen <- observed{r.Method, r.URL.String(), r.Host, string(body), r.Header.Clone()}
		w.Header().Set("Connection", "X-Upstream-Hop, keep-alive")
		w.Header().Set("X-Upstream-Hop", "remove-response-hop")
		w.Header().Set("Proxy-Connection", "keep-alive")
		w.Header().Set("Keep-Alive", "timeout=20")
		w.Header().Set("Proxy-Authenticate", "Basic realm=not-an-origin-header")
		w.Header().Set("X-Origin-Header", "retained")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "origin-response\x00\xff")
	}))
	t.Cleanup(upstream.Close)
	p, server, logs := startTestProxy(t, testProxyConfig(upstream.URL))
	// An explicit trusted upstream must resolve the destination itself.
	p.resolver = testResolver{err: errors.New("local origin resolution is forbidden")}
	client := testHTTPClient(t, server.URL)
	body := "body-secret-31:\x00\xff"
	req, err := http.NewRequest(http.MethodPost,
		"http://api.playstation.net/upload?token=query-secret-42", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "X-Client-Hop, keep-alive")
	req.Header.Set("X-Client-Hop", "remove-request-hop")
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("Keep-Alive", "timeout=20")
	req.Header.Set("Proxy-Authorization", "Basic proxy-secret-53")
	req.Header.Set("Authorization", "Bearer auth-secret-64")
	req.Header.Set("Cookie", "session=cookie-secret-75")
	req.Header.Set("X-End-To-End", "retained")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gotBody := readTestBody(t, response)
	if response.StatusCode != http.StatusCreated || string(gotBody) != "origin-response\x00\xff" {
		t.Fatalf("response changed: status=%d body=%q", response.StatusCode, gotBody)
	}
	for _, name := range []string{"X-Upstream-Hop", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate"} {
		if response.Header.Get(name) != "" {
			t.Errorf("response hop header leaked: %s", name)
		}
	}
	if response.Header.Get("X-Origin-Header") != "retained" {
		t.Error("end-to-end response header lost")
	}
	got := <-seen
	if got.method != http.MethodPost || got.host != "api.playstation.net" || got.body != body {
		t.Errorf("upstream request changed: %+v", got)
	}
	parsed, err := url.Parse(got.target)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "api.playstation.net" ||
		parsed.Path != "/upload" || parsed.RawQuery != "token=query-secret-42" {
		t.Errorf("upstream did not receive original absolute URL: %q", got.target)
	}
	for _, name := range []string{"X-Client-Hop", "Proxy-Connection", "Keep-Alive", "Proxy-Authorization"} {
		if got.header.Get(name) != "" {
			t.Errorf("request hop header leaked: %s", name)
		}
	}
	if got.header.Get("Authorization") != "Bearer auth-secret-64" ||
		got.header.Get("Cookie") != "session=cookie-secret-75" ||
		got.header.Get("X-End-To-End") != "retained" {
		t.Error("origin authorization/cookie/end-to-end header was changed")
	}
	text := waitTestLog(t, logs, "api.playstation.net")
	for _, secret := range []string{"body-secret-31", "query-secret-42", "proxy-secret-53", "auth-secret-64", "cookie-secret-75", "/upload"} {
		if strings.Contains(text, secret) {
			t.Errorf("sensitive data appeared in log: %q", secret)
		}
	}
}

func TestProbeModesAndExactLocal204Scope(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-From-Upstream", "yes")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(upstream.Close)
	tests := []struct {
		name, mode, method, target string
		local                      bool
	}{
		{"default-forward", "forward", "GET", "http://www.google.com/generate_204", false},
		{"gstatic-get", "local204", "GET", "http://connectivitycheck.gstatic.com/generate_204", true},
		{"google-head", "local204", "HEAD", "http://www.google.com/gen_204", true},
		{"android-exact", "local204", "GET", "http://connectivitycheck.android.com/generate_204", true},
		{"post-not-synthetic", "local204", "POST", "http://www.google.com/generate_204", false},
		{"query-not-synthetic", "local204", "GET", "http://www.google.com/generate_204?probe=1", false},
		{"slash-not-synthetic", "local204", "GET", "http://www.google.com/generate_204/", false},
		{"escaped-not-synthetic", "local204", "GET", "http://www.google.com/%67enerate_204", false},
		{"other-google-host", "local204", "GET", "http://other.google.com/generate_204", false},
		{"sony-path-is-real", "local204", "GET", "http://api.playstation.net/generate_204", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testProxyConfig(upstream.URL)
			cfg.ProbeMode = tc.mode
			_, server, _ := startTestProxy(t, cfg)
			before := calls.Load()
			req, err := http.NewRequest(tc.method, tc.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := testHTTPClient(t, server.URL).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body := readTestBody(t, response)
			if tc.local {
				if response.StatusCode != 204 || len(body) != 0 || calls.Load() != before {
					t.Fatalf("local probe: status=%d bytes=%d upstream delta=%d", response.StatusCode, len(body), calls.Load()-before)
				}
				if _, present := response.Header["Content-Length"]; present {
					t.Error("204 must not contain Content-Length")
				}
				if len(response.TransferEncoding) != 0 {
					t.Error("204 must not contain Transfer-Encoding")
				}
			} else if response.StatusCode != 202 || response.Header.Get("X-From-Upstream") != "yes" || calls.Load() != before+1 {
				t.Fatalf("request should reach upstream: status=%d upstream delta=%d", response.StatusCode, calls.Load()-before)
			}
		})
	}
}

func startEchoUpstream(t *testing.T, greeting []byte) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "api.playstation.net:443" {
			http.Error(w, "unexpected tunnel destination", 400)
			return
		}
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(8 * time.Second))
		// Header and binary data in one write expose lost upstream read-ahead.
		packet := append([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"), greeting...)
		if _, err := c.Write(packet); err != nil {
			return
		}
		_, _ = io.Copy(c, rw.Reader)
	}))
	t.Cleanup(s.Close)
	return s
}

func openTestTunnel(t *testing.T, address string, early []byte) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	c, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	header := "CONNECT api.playstation.net:443 HTTP/1.1\r\nHost: api.playstation.net:443\r\n\r\n"
	if _, err := c.Write(append([]byte(header), early...)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return c, reader, response
}

func TestConnectPreservesBinaryAndBufferedData(t *testing.T) {
	greeting := []byte{0x16, 0x03, 0x03, 0, 5, 0xff, 0, '\r', '\n', 0x80}
	upstream := startEchoUpstream(t, greeting)
	_, server, _ := startTestProxy(t, testProxyConfig(upstream.URL))
	early := []byte{0x16, 0x03, 0x01, 0, 6, '\r', '\n', 0, 0xff, 0x80, 0x42}
	c, reader, response := openTestTunnel(t, server.Listener.Addr().String(), early)
	if response.StatusCode != 200 {
		t.Fatalf("CONNECT status=%d", response.StatusCode)
	}
	for _, name := range []string{"Content-Length", "Transfer-Encoding"} {
		if response.Header.Get(name) != "" {
			t.Errorf("CONNECT success has forbidden framing %s", name)
		}
	}
	want := append(append([]byte(nil), greeting...), early...)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("lost pre-read tunnel bytes: got=%x want=%x err=%v", got, want, err)
	}
	payload := bytes.Repeat([]byte{0, 0xff, '\r', '\n', 0x81, 0x42}, 4096)
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, len(payload))
	if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("binary tunnel round trip failed: err=%v bytes=%d", err, len(got))
	}
}

func TestConnectUpstreamFailureNeverReturnsSuccess(t *testing.T) {
	for _, code := range []int{http.StatusProxyAuthRequired, http.StatusForbidden, http.StatusBadGateway} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Proxy-Authenticate", "Basic realm=private")
				http.Error(w, "upstream-private-error-secret", code)
			}))
			t.Cleanup(upstream.Close)
			_, server, logs := startTestProxy(t, testProxyConfig(upstream.URL))
			_, _, response := openTestTunnel(t, server.Listener.Addr().String(), nil)
			body := readTestBody(t, response)
			if response.StatusCode < 400 || response.StatusCode >= 600 {
				t.Fatalf("failed upstream became status %d", response.StatusCode)
			}
			if bytes.Contains(body, []byte("upstream-private-error-secret")) || strings.Contains(logs.String(), "upstream-private-error-secret") {
				t.Error("upstream response body leaked into error/log")
			}
		})
	}
}

func TestTruncatedUpstreamBodyIsNotReportedAsComplete(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 32\r\nConnection: close\r\n\r\nshort")
	}))
	t.Cleanup(upstream.Close)
	_, server, _ := startTestProxy(t, testProxyConfig(upstream.URL))
	response, err := testHTTPClient(t, server.URL).Get("http://api.playstation.net/incomplete")
	if err != nil {
		// Rejecting an incomplete response before it reaches the client is safe.
		return
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if response.StatusCode >= 200 && response.StatusCode < 300 && readErr == nil {
		t.Fatalf("truncated origin response became a successful complete body: %q", body)
	}
}

func TestConnectionLimitRejectsExcessRequest(t *testing.T) {
	upstream := startEchoUpstream(t, nil)
	cfg := testProxyConfig(upstream.URL)
	cfg.MaxConnections = 1
	_, server, _ := startTestProxy(t, cfg)
	_, _, tunnelResponse := openTestTunnel(t, server.Listener.Addr().String(), nil)
	if tunnelResponse.StatusCode != 200 {
		t.Fatalf("initial CONNECT status=%d", tunnelResponse.StatusCode)
	}
	response, err := testHTTPClient(t, server.URL).Get("http://api.playstation.net/second")
	if err != nil {
		t.Fatal(err)
	}
	readTestBody(t, response)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("capacity should reject promptly, got %d", response.StatusCode)
	}
}

func TestCloseTerminatesActiveTunnelAndRejectsNewRequests(t *testing.T) {
	upstream := startEchoUpstream(t, nil)
	p, server, _ := startTestProxy(t, testProxyConfig(upstream.URL))
	c, reader, response := openTestTunnel(t, server.Listener.Addr().String(), nil)
	if response.StatusCode != 200 {
		t.Fatalf("CONNECT status %d", response.StatusCode)
	}
	if _, err := c.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish promptly")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, err := reader.ReadByte()
	if err == nil {
		t.Error("tunnel remained readable after Close")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Error("Close left active tunnel open until the test deadline")
	}
	request := httptest.NewRequest("GET", "http://api.playstation.net/", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	if recorder.Code < 400 {
		t.Errorf("closed proxy served new request: %d", recorder.Code)
	}
	p.Close() // Idempotent shutdown is required by signal/server cleanup paths.
}

func TestRejectsInvalidTargetsMethodsAndClientsBeforeUpstream(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(202)
	}))
	t.Cleanup(upstream.Close)
	p, _, logs := startTestProxy(t, testProxyConfig(upstream.URL))
	tests := []struct {
		name, method, target, remote, host string
	}{
		{"domain-boundary", "GET", "http://evilplaystation.net/", "", ""},
		{"domain-suffix", "GET", "http://playstation.net.example.org/", "", ""},
		{"not-whitelisted", "GET", "http://example.org/", "", ""},
		{"android-subdomain", "GET", "http://evil.connectivitycheck.android.com/generate_204", "", ""},
		{"http-wrong-port", "GET", "http://api.playstation.net:8080/", "", ""},
		{"https-without-connect", "GET", "https://api.playstation.net/", "", ""},
		{"literal-ip", "GET", "http://127.0.0.1/", "", ""},
		{"url-credentials", "GET", "http://user:request-secret@api.playstation.net/", "", ""},
		{"host-disagreement", "GET", "http://api.playstation.net/", "", "other.google.com"},
		{"client-not-allowed", "GET", "http://api.playstation.net/", "192.0.2.10:54321", ""},
		{"client-malformed", "GET", "http://api.playstation.net/", "not-an-ip", ""},
		{"trace", "TRACE", "http://api.playstation.net/", "", ""},
		{"unsupported-method", "BREW", "http://api.playstation.net/", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, nil)
			r.RemoteAddr = tc.remote
			if r.RemoteAddr == "" {
				r.RemoteAddr = "127.0.0.1:12345"
			}
			if tc.host != "" {
				r.Host = tc.host
			}
			before := calls.Load()
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code < 400 || w.Code >= 500 {
				t.Errorf("expected client rejection, got %d", w.Code)
			}
			if calls.Load() != before {
				t.Error("rejected request reached upstream")
			}
		})
	}
	for _, authority := range []string{"api.playstation.net", "api.playstation.net:80", "api.playstation.net:25", "api.playstation.net:0", "api.playstation.net:65536", "[::1]:443"} {
		t.Run("CONNECT/"+authority, func(t *testing.T) {
			r := &http.Request{Method: "CONNECT", URL: &url.URL{Host: authority}, Host: authority,
				RequestURI: authority, Header: make(http.Header), RemoteAddr: "127.0.0.1:12345"}
			before := calls.Load()
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code < 400 || w.Code >= 500 || calls.Load() != before {
				t.Errorf("invalid CONNECT escaped validation: status=%d upstream=%d", w.Code, calls.Load()-before)
			}
		})
	}
	if strings.Contains(logs.String(), "request-secret") {
		t.Error("rejected target credentials leaked to log")
	}
}

type testResolver struct {
	addresses []net.IPAddr
	err       error
	calls     *atomic.Int64
}

func (r testResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	if r.calls != nil {
		r.calls.Add(1)
	}
	return r.addresses, r.err
}

func TestDirectDNSRejectsNonPublicAnswersWithoutDialing(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "192.168.88.1", "169.254.169.254", "100.64.1.1", "198.18.1.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "2001:db8::1"} {
		t.Run(address, func(t *testing.T) {
			p, _, _ := startTestProxy(t, testProxyConfig(""))
			// A mixed answer must be rejected entirely, before trying the public IP.
			p.resolver = testResolver{addresses: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP(address)}}}
			var dials atomic.Int64
			p.dialContext = func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("test forbids external dialing")
			}
			r := httptest.NewRequest("GET", "http://api.playstation.net/", nil)
			r.RemoteAddr = "127.0.0.1:12345"
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code < 400 || dials.Load() != 0 {
				t.Fatalf("private DNS result was dialed: status=%d dials=%d", w.Code, dials.Load())
			}
		})
	}
}

func TestDirectDNSDialsValidatedIPWithoutSecondResolution(t *testing.T) {
	p, _, _ := startTestProxy(t, testProxyConfig(""))
	var resolutions atomic.Int64
	p.resolver = testResolver{addresses: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, calls: &resolutions}
	dialed := make(chan string, 1)
	p.dialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		dialed <- network + " " + address
		client, origin := net.Pipe()
		go func() {
			defer origin.Close()
			_ = origin.SetDeadline(time.Now().Add(2 * time.Second))
			request, err := http.ReadRequest(bufio.NewReader(origin))
			if err != nil {
				return
			}
			request.Body.Close()
			_, _ = io.WriteString(origin, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
		}()
		return client, nil
	}
	r := httptest.NewRequest("GET", "http://api.playstation.net/", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("simulated direct origin status=%d", w.Code)
	}
	if got := <-dialed; got != "tcp 8.8.8.8:80" || resolutions.Load() != 1 {
		t.Errorf("expected one lookup and validated IP dial: target=%q lookups=%d", got, resolutions.Load())
	}
}

func TestConfigDefaultsRoundTripAndValidation(t *testing.T) {
	defaults, err := LoadConfig("")
	if err != nil || defaults.ProbeMode != "forward" {
		t.Fatalf("safe default probe mode: cfg=%+v err=%v", defaults, err)
	}
	host, _, err := net.SplitHostPort(defaults.Listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("default listener must be loopback: %q", defaults.Listen)
	}
	tests := []struct{ name, text string }{
		{"null", `null`}, {"empty", ``}, {"array", `[]`},
		{"unknown-key", `{"proxy_password":"config-secret-91"}`},
		{"trailing-json", `{} {}`}, {"broken-json", `{"listen":`},
		{"mode", `{"probe_mode":"always-success"}`},
		{"empty-clients", `{"allowed_clients":[]}`},
		{"empty-domains", `{"allowed_domains":[]}`},
		{"bad-cidr", `{"allowed_clients":["127.0.0.1"]}`},
		{"wildcard", `{"allowed_domains":["*.playstation.net"]}`},
		{"literal-domain", `{"allowed_domains":["127.0.0.1"]}`},
		{"bad-listener", `{"listen":"localhost:18081"}`},
		{"dial-timeout", `{"dial_timeout_seconds":-1}`},
		{"idle-timeout", `{"idle_timeout_seconds":-1}`},
		{"connections", `{"max_connections":-1}`},
		{"upstream-credentials", `{"upstream":"http://user:config-secret-91@127.0.0.1:7890"}`},
		{"upstream-hostname", `{"upstream":"http://proxy.example.org:7890"}`},
		{"upstream-socks", `{"upstream":"socks5://127.0.0.1:7890"}`},
		{"upstream-no-port", `{"upstream":"http://127.0.0.1"}`},
		{"upstream-path", `{"upstream":"http://127.0.0.1:7890/path"}`},
		{"upstream-query", `{"upstream":"http://127.0.0.1:7890?secret=config-secret-91"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("invalid config was accepted")
			}
			if strings.Contains(err.Error(), "config-secret-91") {
				t.Error("config error echoed a secret")
			}
		})
	}
	cfg := testProxyConfig("http://127.0.0.1:7890")
	cfg.ProbeMode = "local204"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "valid.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || got.Upstream != cfg.Upstream || got.ProbeMode != cfg.ProbeMode || got.Listen != cfg.Listen {
		t.Fatalf("valid configuration failed round trip: %+v %v", got, err)
	}
	cfg.MaxConnections = -1
	if p, err := NewProxy(cfg, nil); err == nil {
		p.Close()
		t.Error("NewProxy accepted invalid configuration")
	}
}
