package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxUpstreamResponseHeaderBytes = 64 << 10

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type Proxy struct {
	cfg         Config
	logger      *log.Logger
	clients     []netip.Prefix
	upstream    *url.URL
	transport   *http.Transport
	resolver    ipResolver
	dialContext func(context.Context, string, string) (net.Conn, error)
	semaphore   chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	tunnels     map[net.Conn]struct{}
}

func NewProxy(cfg Config, logger *log.Logger) (*Proxy, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Proxy{cfg: cfg, logger: logger, resolver: net.DefaultResolver,
		semaphore: make(chan struct{}, cfg.MaxConnections), ctx: ctx, cancel: cancel,
		tunnels: make(map[net.Conn]struct{})}
	for _, raw := range cfg.AllowedClients {
		prefix, _ := netip.ParsePrefix(raw)
		p.clients = append(p.clients, prefix)
	}
	dialer := &net.Dialer{Timeout: p.dialTimeout(), KeepAlive: 30 * time.Second}
	p.dialContext = dialer.DialContext
	if cfg.Upstream != "" {
		p.upstream, _ = parseUpstream(cfg.Upstream)
	}
	p.transport = &http.Transport{
		Proxy: nil, DialContext: p.transportDial,
		ForceAttemptHTTP2: false, DisableCompression: true,
		MaxIdleConns: cfg.MaxConnections, MaxIdleConnsPerHost: 8,
		MaxConnsPerHost: cfg.MaxConnections,
		IdleConnTimeout: p.idleTimeout(), ResponseHeaderTimeout: p.idleTimeout(),
		TLSHandshakeTimeout: p.dialTimeout(), ExpectContinueTimeout: time.Second,
		MaxResponseHeaderBytes: maxUpstreamResponseHeaderBytes,
	}
	if p.upstream != nil {
		p.transport.Proxy = http.ProxyURL(p.upstream)
	}
	return p, nil
}

func (p *Proxy) dialTimeout() time.Duration {
	return time.Duration(p.cfg.DialTimeoutSeconds) * time.Second
}
func (p *Proxy) idleTimeout() time.Duration {
	return time.Duration(p.cfg.IdleTimeoutSeconds) * time.Second
}

func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	connections := make([]net.Conn, 0, len(p.tunnels))
	for c := range p.tunnels {
		connections = append(connections, c)
	}
	p.mu.Unlock()
	p.cancel()
	for _, c := range connections {
		_ = c.Close()
	}
	p.transport.CloseIdleConnections()
	return nil
}

func (p *Proxy) clientAllowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range p.clients {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (p *Proxy) domainAllowed(host string) bool {
	for _, domain := range p.cfg.AllowedDomains {
		if host == domain {
			return true
		}
		if domain != "connectivitycheck.android.com" && strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func requestTarget(r *http.Request) (string, string, error) {
	if r.URL == nil || r.URL.User != nil || r.URL.Opaque != "" || r.URL.Fragment != "" || r.URL.RawFragment != "" {
		return "", "", errors.New("invalid proxy request target")
	}
	port := "80"
	connect := r.Method == http.MethodConnect
	if connect {
		port = "443"
		if r.URL.Scheme != "" || r.URL.Path != "" || r.URL.RawQuery != "" || r.URL.ForceQuery ||
			r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
			return "", "", errors.New("invalid CONNECT request")
		}
	} else if r.URL.Scheme != "http" || !r.URL.IsAbs() {
		return "", "", errors.New("absolute HTTP URL required")
	}
	host, _, err := canonicalAuthority(r.URL.Host, port, connect)
	if err != nil {
		return "", "", err
	}
	other, _, err := canonicalAuthority(r.Host, port, connect)
	if err != nil || other != host {
		return "", "", errors.New("ambiguous target authority")
	}
	if connect {
		other, _, err = canonicalAuthority(r.RequestURI, port, true)
		if err != nil || other != host {
			return "", "", errors.New("ambiguous CONNECT target")
		}
	}
	if values, ok := r.Header["Host"]; ok {
		if len(values) != 1 {
			return "", "", errors.New("ambiguous Host header")
		}
		other, _, err = canonicalAuthority(values[0], port, connect)
		if err != nil || other != host {
			return "", "", errors.New("ambiguous Host header")
		}
	}
	return host, port, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	host, port, outcome := "invalid", "0", "rejected"
	var sent, received int64
	defer func() {
		p.logger.Printf("target=%s port=%s result=%s duration_ms=%d bytes_up=%d bytes_down=%d", host, port, outcome, time.Since(started).Milliseconds(), sent, received)
	}()
	if !p.clientAllowed(r.RemoteAddr) {
		outcome = "client_denied"
		proxyError(w, http.StatusForbidden, "Client is not allowed")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect:
	default:
		outcome = "method_denied"
		proxyError(w, http.StatusMethodNotAllowed, "HTTP method is not supported")
		return
	}
	var err error
	host, port, err = requestTarget(r)
	if err != nil {
		host, port, outcome = "invalid", "0", "invalid_target"
		proxyError(w, http.StatusBadRequest, "Invalid proxy target")
		return
	}
	if !p.domainAllowed(host) {
		outcome = "domain_denied"
		proxyError(w, http.StatusForbidden, "Target is not allowed")
		return
	}
	select {
	case p.semaphore <- struct{}{}:
		defer func() { <-p.semaphore }()
	default:
		outcome = "capacity"
		proxyError(w, http.StatusServiceUnavailable, "Proxy connection limit reached")
		return
	}
	select {
	case <-p.ctx.Done():
		outcome = "closed"
		proxyError(w, http.StatusServiceUnavailable, "Proxy is shutting down")
		return
	default:
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(p.ctx, cancel)
	defer func() { stop(); cancel() }()
	r = r.WithContext(ctx)
	if r.Method == http.MethodConnect {
		sent, received, outcome = p.serveConnect(w, r, host, port)
		return
	}
	if p.localProbe(r, host) {
		outcome = "local204"
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sent, received, outcome = p.serveForward(w, r, host, port)
	if outcome == "client_write_failed" || outcome == "target_read_failed" {
		// Headers are already committed. A normal return could make a truncated
		// streamed response appear complete; abort the downstream connection.
		panic(http.ErrAbortHandler)
	}
}

func proxyError(w http.ResponseWriter, code int, message string) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, message, code)
}

func (p *Proxy) localProbe(r *http.Request, host string) bool {
	if p.cfg.ProbeMode != "local204" || (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
		r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.EscapedPath() != r.URL.Path || r.ContentLength > 0 || len(r.TransferEncoding) != 0 {
		return false
	}
	if r.URL.Path != "/generate_204" && r.URL.Path != "/gen_204" {
		return false
	}
	switch host {
	case "connectivitycheck.gstatic.com", "connectivitycheck.android.com", "www.google.com", "clients3.google.com":
		return true
	}
	return false
}

func stripHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, field := range strings.Split(value, ",") {
			if field = strings.TrimSpace(field); field != "" {
				h.Del(field)
			}
		}
	}
	for _, field := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(field)
	}
}

type countedBody struct {
	io.ReadCloser
	controller *http.ResponseController
	idle       time.Duration
	count      atomic.Int64
}

func (b *countedBody) Read(dst []byte) (int, error) {
	_ = b.controller.SetReadDeadline(time.Now().Add(b.idle))
	n, err := b.ReadCloser.Read(dst)
	b.count.Add(int64(n))
	return n, err
}

func (p *Proxy) serveForward(w http.ResponseWriter, r *http.Request, host, port string) (int64, int64, string) {
	controller := http.NewResponseController(w)
	defer func() { _ = controller.SetReadDeadline(time.Time{}); _ = controller.SetWriteDeadline(time.Time{}) }()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Host = net.JoinHostPort(host, port)
	out.Host = host
	out.Header = r.Header.Clone()
	stripHopHeaders(out.Header)
	out.Close, out.Trailer, out.TransferEncoding, out.GetBody = false, nil, nil, nil
	var tracked *countedBody
	if out.Body != nil && out.Body != http.NoBody {
		tracked = &countedBody{ReadCloser: out.Body, controller: controller, idle: p.idleTimeout()}
		out.Body = tracked
	}
	countUp := func() int64 {
		if tracked != nil {
			return tracked.count.Load()
		}
		return 0
	}
	response, err := p.transport.RoundTrip(out)
	if err != nil {
		proxyError(w, gatewayStatus(err), "Target connection failed")
		return countUp(), 0, "forward_failed"
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		proxyError(w, http.StatusBadGateway, "Protocol upgrades are not supported")
		return countUp(), 0, "upgrade_denied"
	}
	stripHopHeaders(response.Header)
	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	_ = controller.SetWriteDeadline(time.Now().Add(p.idleTimeout()))
	w.WriteHeader(response.StatusCode)
	buffer := make([]byte, 32<<10)
	var countDown int64
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			_ = controller.SetWriteDeadline(time.Now().Add(p.idleTimeout()))
			written, writeErr := w.Write(buffer[:n])
			countDown += int64(written)
			if writeErr != nil || written != n {
				return countUp(), countDown, "client_write_failed"
			}
			if err := controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return countUp(), countDown, "client_write_failed"
			}
		}
		if readErr == io.EOF {
			return countUp(), countDown, "forwarded"
		}
		if readErr != nil {
			return countUp(), countDown, "target_read_failed"
		}
	}
}

func gatewayStatus(err error) int {
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func (p *Proxy) transportDial(ctx context.Context, network, address string) (net.Conn, error) {
	var c net.Conn
	var err error
	if p.upstream != nil {
		if address != p.upstream.Host {
			return nil, errors.New("unexpected upstream endpoint")
		}
		bounded, cancel := context.WithTimeout(ctx, p.dialTimeout())
		defer cancel()
		c, err = p.dialContext(bounded, "tcp", p.upstream.Host)
	} else {
		host, port, parseErr := canonicalAuthority(address, "80", true)
		if parseErr != nil || !p.domainAllowed(host) {
			return nil, errors.New("invalid direct target")
		}
		c, err = p.dialPublic(ctx, host, port)
	}
	if err != nil {
		return nil, err
	}
	activity := &connectionActivity{connections: []net.Conn{c}, timeout: p.idleTimeout()}
	activity.touch()
	return &activityConn{Conn: c, activity: activity}, nil
}

var forbiddenNetworks = func() []netip.Prefix {
	raw := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10",
	}
	result := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}()

func publicAddress(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range forbiddenNetworks {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func (p *Proxy) dialPublic(ctx context.Context, host, port string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, p.dialTimeout())
	defer cancel()
	addresses, err := p.resolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("target DNS resolution failed")
	}
	for _, addr := range addresses {
		if addr.Zone != "" || !publicAddress(addr.IP) {
			return nil, errors.New("target DNS address is not public")
		}
	}
	for _, addr := range addresses {
		// Dial the validated address itself, never resolve the target a second time.
		c, err := p.dialContext(ctx, "tcp", net.JoinHostPort(addr.IP.String(), port))
		if err == nil {
			return c, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("target connection failed")
}

func (p *Proxy) dialTunnel(ctx context.Context, host, port string) (net.Conn, error) {
	if p.upstream == nil {
		return p.dialPublic(ctx, host, port)
	}
	ctx, cancel := context.WithTimeout(ctx, p.dialTimeout())
	defer cancel()
	c, err := p.dialContext(ctx, "tcp", p.upstream.Host)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = c.Close()
		}
	}()
	_ = c.SetDeadline(time.Now().Add(p.dialTimeout()))
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	authority := net.JoinHostPort(host, port)
	if _, err := io.WriteString(c, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		return nil, err
	}
	limited := &headerLimitedReader{reader: c, remaining: maxUpstreamResponseHeaderBytes}
	reader := bufio.NewReader(limited)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("upstream CONNECT failed")
	}
	limited.unlimited = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	ok = true
	return &bufferedConn{Conn: c, reader: reader}, nil
}

type headerLimitedReader struct {
	reader    io.Reader
	remaining int
	unlimited bool
}

func (r *headerLimitedReader) Read(dst []byte) (int, error) {
	if r.unlimited {
		return r.reader.Read(dst)
	}
	if r.remaining <= 0 {
		return 0, errors.New("upstream response headers too large")
	}
	if len(dst) > r.remaining {
		dst = dst[:r.remaining]
	}
	n, err := r.reader.Read(dst)
	r.remaining -= n
	return n, err
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(dst []byte) (int, error) { return c.reader.Read(dst) }

type connectionActivity struct {
	mu          sync.Mutex
	connections []net.Conn
	timeout     time.Duration
}

func (a *connectionActivity) touch() {
	a.mu.Lock()
	defer a.mu.Unlock()
	deadline := time.Now().Add(a.timeout)
	for _, c := range a.connections {
		_ = c.SetDeadline(deadline)
	}
}

type activityConn struct {
	net.Conn
	activity *connectionActivity
}

func (c *activityConn) Read(dst []byte) (int, error) {
	c.activity.touch()
	n, err := c.Conn.Read(dst)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}
func (c *activityConn) Write(src []byte) (int, error) {
	c.activity.touch()
	n, err := c.Conn.Write(src)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}

func closeWrite(c net.Conn) {
	switch value := c.(type) {
	case *activityConn:
		closeWrite(value.Conn)
	case *bufferedConn:
		closeWrite(value.Conn)
	default:
		if writer, ok := c.(interface{ CloseWrite() error }); ok {
			_ = writer.CloseWrite()
		}
	}
}

func (p *Proxy) registerTunnel(first, second net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.tunnels[first], p.tunnels[second] = struct{}{}, struct{}{}
	return true
}
func (p *Proxy) removeTunnel(first, second net.Conn) {
	p.mu.Lock()
	delete(p.tunnels, first)
	delete(p.tunnels, second)
	p.mu.Unlock()
}

func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request, host, port string) (int64, int64, string) {
	upstream, err := p.dialTunnel(r.Context(), host, port)
	if err != nil {
		proxyError(w, gatewayStatus(err), "Target connection failed")
		return 0, 0, "connect_failed"
	}
	defer upstream.Close()
	client, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		proxyError(w, http.StatusInternalServerError, "Tunnelling is unavailable")
		return 0, 0, "hijack_failed"
	}
	defer client.Close()
	if !p.registerTunnel(client, upstream) {
		return 0, 0, "closed"
	}
	defer p.removeTunnel(client, upstream)
	_ = client.SetDeadline(time.Now().Add(p.idleTimeout()))
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return 0, 0, "client_write_failed"
	}
	if err := buffered.Flush(); err != nil {
		return 0, 0, "client_write_failed"
	}
	activity := &connectionActivity{connections: []net.Conn{client, upstream}, timeout: p.idleTimeout()}
	activity.touch()
	left := &activityConn{Conn: &bufferedConn{Conn: client, reader: buffered.Reader}, activity: activity}
	right := &activityConn{Conn: upstream, activity: activity}
	type copyResult struct {
		upload bool
		bytes  int64
		err    error
	}
	done := make(chan copyResult, 2)
	copyHalf := func(dst, src net.Conn, upload bool) {
		n, err := io.CopyBuffer(dst, src, make([]byte, 32<<10))
		if err == nil {
			closeWrite(dst)
		} else {
			_ = client.Close()
			_ = upstream.Close()
		}
		done <- copyResult{upload: upload, bytes: n, err: err}
	}
	go copyHalf(right, left, true)
	go copyHalf(left, right, false)
	var sent, received int64
	outcome := "tunnel_closed"
	for i := 0; i < 2; i++ {
		result := <-done
		if result.upload {
			sent = result.bytes
		} else {
			received = result.bytes
		}
		if result.err != nil {
			outcome = "tunnel_ended"
		}
	}
	return sent, received, outcome
}
