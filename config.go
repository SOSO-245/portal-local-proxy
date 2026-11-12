package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Listen             string   `json:"listen"`
	AllowedClients     []string `json:"allowed_clients"`
	AllowedDomains     []string `json:"allowed_domains"`
	ProbeMode          string   `json:"probe_mode"`
	Upstream           string   `json:"upstream"`
	DialTimeoutSeconds int      `json:"dial_timeout_seconds"`
	IdleTimeoutSeconds int      `json:"idle_timeout_seconds"`
	MaxConnections     int      `json:"max_connections"`
}

func DefaultConfig() Config {
	return Config{
		Listen:         "127.0.0.1:18081",
		AllowedClients: []string{"127.0.0.1/32", "::1/128"},
		AllowedDomains: []string{
			"playstation.com", "playstation.net", "sony.com",
			"sonyentertainmentnetwork.com", "google.com", "googleapis.com",
			"gstatic.com", "connectivitycheck.android.com",
		},
		ProbeMode: "forward", DialTimeoutSeconds: 10,
		IdleTimeoutSeconds: 300, MaxConnections: 64,
	}
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot open configuration file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return Config{}, errors.New("configuration file is unreadable or too large")
	}
	if trimmed := strings.TrimSpace(string(data)); len(trimmed) == 0 || trimmed[0] != '{' {
		return Config{}, errors.New("configuration must contain a JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, errors.New("configuration must be valid JSON with supported fields")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("configuration must contain exactly one JSON object")
	}
	return normalizeConfig(cfg)
}

func normalizeConfig(cfg Config) (Config, error) {
	d := DefaultConfig()
	if cfg.Listen == "" {
		cfg.Listen = d.Listen
	}
	if cfg.AllowedClients == nil {
		cfg.AllowedClients = d.AllowedClients
	}
	if cfg.AllowedDomains == nil {
		cfg.AllowedDomains = d.AllowedDomains
	}
	if cfg.ProbeMode == "" {
		cfg.ProbeMode = d.ProbeMode
	}
	if cfg.DialTimeoutSeconds == 0 {
		cfg.DialTimeoutSeconds = d.DialTimeoutSeconds
	}
	if cfg.IdleTimeoutSeconds == 0 {
		cfg.IdleTimeoutSeconds = d.IdleTimeoutSeconds
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = d.MaxConnections
	}
	if cfg.DialTimeoutSeconds < 1 || cfg.DialTimeoutSeconds > 120 {
		return Config{}, errors.New("dial_timeout_seconds must be between 1 and 120")
	}
	if cfg.IdleTimeoutSeconds < 1 || cfg.IdleTimeoutSeconds > 3600 {
		return Config{}, errors.New("idle_timeout_seconds must be between 1 and 3600")
	}
	if cfg.MaxConnections < 1 || cfg.MaxConnections > 4096 {
		return Config{}, errors.New("max_connections must be between 1 and 4096")
	}
	if cfg.ProbeMode != "forward" && cfg.ProbeMode != "local204" {
		return Config{}, errors.New("probe_mode must be forward or local204")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || net.ParseIP(host) == nil || !validConfigPort(port) {
		return Config{}, errors.New("listen must be an IP address and port")
	}
	if len(cfg.AllowedClients) == 0 || len(cfg.AllowedDomains) == 0 {
		return Config{}, errors.New("client and domain allowlists must not be empty")
	}
	clients := make([]string, 0, len(cfg.AllowedClients))
	for _, raw := range cfg.AllowedClients {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() {
			return Config{}, errors.New("allowed_clients must contain valid IPv4 or IPv6 CIDRs")
		}
		clients = append(clients, prefix.Masked().String())
	}
	domains := make([]string, 0, len(cfg.AllowedDomains))
	for _, raw := range cfg.AllowedDomains {
		domain := strings.ToLower(raw)
		if !validDomain(domain) || net.ParseIP(domain) != nil {
			return Config{}, errors.New("allowed_domains must contain DNS names without wildcards or ports")
		}
		// The Android connectivity exception is deliberately a single host.
		if domain == "android.com" {
			return Config{}, errors.New("use the exact connectivitycheck.android.com host instead of android.com")
		}
		domains = append(domains, domain)
	}
	cfg.AllowedClients, cfg.AllowedDomains = clients, domains
	if cfg.Upstream != "" {
		if _, err := parseUpstream(cfg.Upstream); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func validConfigPort(raw string) bool {
	n, err := strconv.Atoi(raw)
	return err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == raw
}

func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" ||
		u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, errors.New("upstream must be an http://IP:port URL without credentials, path, query or fragment")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || net.ParseIP(host) == nil || !validConfigPort(port) {
		return nil, errors.New("upstream must use an explicit IP address and port")
	}
	u.Host = net.JoinHostPort(host, port)
	return u, nil
}

func validDomain(host string) bool {
	if host == "" || len(host) > 253 || strings.HasSuffix(host, ".") || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func canonicalAuthority(raw, expectedPort string, requirePort bool) (string, string, error) {
	host, port := raw, expectedPort
	if strings.Contains(raw, ":") {
		var err error
		host, port, err = net.SplitHostPort(raw)
		if err != nil {
			return "", "", errors.New("invalid target authority")
		}
	} else if requirePort {
		return "", "", errors.New("explicit target port required")
	}
	host = strings.ToLower(host)
	if port != expectedPort || net.ParseIP(host) != nil || !validDomain(host) {
		return "", "", fmt.Errorf("target must be a DNS name on port %s", expectedPort)
	}
	return host, port, nil
}
