// Package xray builds Xray configuration files from the parsed VLESS server
// and user-supplied routing rules.
//
// We intentionally never manage Xray's DNS module (02_dns.json). Earlier
// attempts to route DoH through xray — even with the DoH HTTPS endpoint
// pinned to the `direct` outbound to break the obvious deadlock — left the
// router unresponsive ~3 minutes after every boot. Xray's DNS module
// appears to accumulate state on this hardware that the kernel can't keep
// up with. DNS bypass for poisoned domains is handled at install time by
// pointing Keenetic's own DNS upstream at Cloudflare DoH.
package xray

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/voledyaev/yonder/internal/vless"
)

// XKeen runs xray with `-confdir /opt/etc/xray/configs/` — every .json
// in that directory is merged. The default XKeen install ships six files
// (01_log..06_policy). We only own 04_outbounds and 05_routing; we leave
// 01_log/02_dns/03_inbounds/06_policy at XKeen's tested defaults — that
// way we don't have to fight XKeen's tproxy/iptables setup.
const (
	XKeenConfigsDir = "/opt/etc/xray/configs"
	OutboundsFile   = "04_outbounds.json"
	RoutingFile     = "05_routing.json"
)

// BuildOutbound returns the `proxy` outbound for a VLESS server.
// Supports VLESS over Reality (most common today) and plain TLS as a
// fallback. WS/gRPC transports are wired through if `type` indicates so.
func BuildOutbound(srv vless.Server) map[string]any {
	p := srv.Params
	if p == nil {
		p = map[string]string{}
	}

	user := map[string]any{
		"id":         srv.UUID,
		"encryption": "none",
	}
	if v := p["flow"]; v != "" {
		user["flow"] = v
	}

	network := stringOr(p["type"], "tcp")
	stream := map[string]any{
		"network": network,
	}
	security := stringOr(p["security"], "none")
	stream["security"] = security

	switch security {
	case "reality":
		stream["realitySettings"] = map[string]any{
			"serverName":  p["sni"],
			"fingerprint": stringOr(p["fp"], "chrome"),
			"publicKey":   p["pbk"],
			"shortId":     p["sid"],
			// Some providers set `spx` in the link to control the post-
			// handshake disguise GET that Reality issues against the SNI
			// host. Honor it; default to no extra step.
			"spiderX": p["spx"],
		}
	case "tls":
		serverName := p["sni"]
		if serverName == "" {
			serverName = p["host"]
		}
		stream["tlsSettings"] = map[string]any{
			"serverName":  serverName,
			"fingerprint": stringOr(p["fp"], "chrome"),
			"alpn":        []string{"h2", "http/1.1"},
		}
	}

	switch network {
	case "ws":
		ws := map[string]any{
			"path": stringOr(p["path"], "/"),
		}
		if h := p["host"]; h != "" {
			ws["headers"] = map[string]string{"Host": h}
		} else {
			ws["headers"] = map[string]string{}
		}
		stream["wsSettings"] = ws
	case "grpc":
		stream["grpcSettings"] = map[string]any{
			"serviceName": trimLeadingSlash(p["path"]),
		}
	}

	return map[string]any{
		"tag":      "proxy",
		"protocol": "vless",
		"settings": map[string]any{
			"vnext": []map[string]any{{
				"address": srv.Host,
				"port":    srv.Port,
				"users":   []map[string]any{user},
			}},
		},
		"streamSettings": stream,
	}
}

// defaultRules is the conservative bundled fallback: "everything through
// the VPN, only RFC-1918 / loopback / link-local goes direct". Raw CIDRs
// are used (not `geoip:private`) so we don't depend on geoip.dat.
func defaultRules() []json.RawMessage {
	rule := map[string]any{
		"type":        "field",
		"outboundTag": "direct",
		"ip": []string{
			"10.0.0.0/8",     // RFC 1918
			"172.16.0.0/12",  // RFC 1918
			"192.168.0.0/16", // RFC 1918
			"127.0.0.0/8",    // Loopback
			"169.254.0.0/16", // Link-local
			"100.64.0.0/10",  // CGNAT
			"224.0.0.0/4",    // Multicast
			"::1/128",        // IPv6 loopback
			"fc00::/7",       // IPv6 ULA
			"fe80::/10",      // IPv6 link-local
			"ff00::/8",       // IPv6 multicast
		},
	}
	raw, _ := json.Marshal(rule)
	return []json.RawMessage{raw}
}

// WriteXKeenSplit writes the two files we own (04_outbounds, 05_routing)
// into XKeen's configs directory. If srv is nil, the proxy outbound is
// omitted and traffic falls through to direct — defensive fallback; the
// daemon should also stop xkeen when vpn_on is false.
//
// rules may be nil — defaultRules() is used in that case.
func WriteXKeenSplit(srv *vless.Server, rules []json.RawMessage, configsDir string) error {
	if configsDir == "" {
		configsDir = XKeenConfigsDir
	}

	var outbounds []map[string]any
	if srv != nil {
		outbounds = append(outbounds, BuildOutbound(*srv))
	}
	outbounds = append(outbounds,
		map[string]any{
			"tag":      "direct",
			"protocol": "freedom",
			"streamSettings": map[string]any{
				"sockopt": map[string]any{"mark": 255},
			},
		},
		map[string]any{
			"tag":      "block",
			"protocol": "blackhole",
		},
	)

	if len(rules) == 0 {
		rules = defaultRules()
	}

	if err := atomicWriteJSON(filepath.Join(configsDir, OutboundsFile),
		map[string]any{"outbounds": outbounds}); err != nil {
		return fmt.Errorf("write outbounds: %w", err)
	}
	if err := atomicWriteJSON(filepath.Join(configsDir, RoutingFile),
		map[string]any{
			"routing": map[string]any{
				"domainStrategy": "AsIs",
				"rules":          rules,
			},
		}); err != nil {
		return fmt.Errorf("write routing: %w", err)
	}
	return nil
}

func atomicWriteJSON(path string, obj any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func stringOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func trimLeadingSlash(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s
}
