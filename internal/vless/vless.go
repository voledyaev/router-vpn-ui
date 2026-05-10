// Package vless parses VLESS subscriptions into structured Server values.
//
// Most providers serve subscriptions as a base64-encoded list of vless://
// URIs separated by newlines; some serve plaintext. Both forms are accepted.
//
// Reference: docs/vless-format.md.
package vless

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Server is a parsed VLESS endpoint. ID is "host:port" and is the stable
// key used by the rest of the app (state, UI, xray config).
type Server struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`    // raw fragment text, e.g. "🇵🇱⚡Польша"
	Country string            `json:"country"` // ISO 3166-1 alpha-2, "??" if unknown
	Host    string            `json:"host"`
	Port    int               `json:"port"`
	UUID    string            `json:"uuid"`
	Params  map[string]string `json:"params"` // transport / crypto params, only present-and-meaningful ones
}

// Country aliases (lower-case): English plus the non-English (Cyrillic)
// aliases that commonly appear in fragments served to non-English-speaking
// users. Fallback only — flag emoji detection runs first.
var countryAliases = map[string]string{
	"польша": "PL", "poland": "PL",
	"испания": "ES", "spain": "ES",
	"германия": "DE", "germany": "DE",
	"венгрия": "HU", "hungary": "HU",
	"италия": "IT", "italy": "IT",
	"нидерланды": "NL", "голландия": "NL", "netherlands": "NL",
	"финляндия": "FI", "finland": "FI",
	"франция": "FR", "france": "FR",
	"великобритания": "GB", "англия": "GB", "uk": "GB", "united kingdom": "GB",
	"сша": "US", "usa": "US", "united states": "US", "america": "US",
	"швеция": "SE", "sweden": "SE",
	"норвегия": "NO", "norway": "NO",
	"дания": "DK", "denmark": "DK",
	"австрия": "AT", "austria": "AT",
	"швейцария": "CH", "switzerland": "CH",
	"бельгия": "BE", "belgium": "BE",
	"чехия": "CZ", "czech": "CZ",
	"словакия": "SK", "slovakia": "SK",
	"румыния": "RO", "romania": "RO",
	"болгария": "BG", "bulgaria": "BG",
	"молдова": "MD", "moldova": "MD",
	"украина": "UA", "ukraine": "UA",
	"казахстан": "KZ", "kazakhstan": "KZ",
	"армения": "AM", "armenia": "AM",
	"грузия": "GE", "georgia": "GE",
	"турция": "TR", "turkey": "TR",
	"япония": "JP", "japan": "JP",
	"корея": "KR", "south korea": "KR",
	"сингапур": "SG", "singapore": "SG",
	"гонконг": "HK", "hong kong": "HK",
	"канада": "CA", "canada": "CA",
	"австралия": "AU", "australia": "AU",
	"литва": "LT", "lithuania": "LT",
	"латвия": "LV", "latvia": "LV",
	"эстония": "EE", "estonia": "EE",
}

var (
	nonWordRE     = regexp.MustCompile(`[^\p{L}\p{N}_\s-]+`)
	whitespaceRE  = regexp.MustCompile(`\s+`)
	whitespaceAll = regexp.MustCompile(`\s`)
)

// flagToCountry extracts a country code from a leading regional-indicator
// flag emoji. Flag emoji are pairs of code points in U+1F1E6..U+1F1FF;
// each pair maps to two ASCII letters via (cp - 0x1F1E6 + 0x41).
func flagToCountry(text string) string {
	runes := []rune(text)
	if len(runes) < 2 {
		return ""
	}
	a, b := runes[0], runes[1]
	const base = 0x1F1E6
	const upper = 0x1F1FF
	if base <= a && a <= upper && base <= b && b <= upper {
		return string([]rune{a - base + 'A', b - base + 'A'})
	}
	return ""
}

// nameToCountry looks up a country code from a free-form fragment by name,
// in any language listed in countryAliases.
func nameToCountry(text string) string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = nonWordRE.ReplaceAllString(s, " ")
	s = whitespaceRE.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if c, ok := countryAliases[s]; ok {
		return c
	}
	for _, w := range strings.Fields(s) {
		if c, ok := countryAliases[w]; ok {
			return c
		}
	}
	return ""
}

// DetectCountry returns an ISO-3166 alpha-2 country code parsed from the
// fragment text, or "??" if undetectable.
func DetectCountry(fragment string) string {
	if fragment == "" {
		return "??"
	}
	if c := flagToCountry(fragment); c != "" {
		return c
	}
	if c := nameToCountry(fragment); c != "" {
		return c
	}
	return "??"
}

// ParseLink parses a single vless://... URI into a Server.
func ParseLink(uri string) (Server, error) {
	if !strings.HasPrefix(uri, "vless://") {
		return Server{}, fmt.Errorf("not a vless URI: %s", truncate(uri, 80))
	}

	u, err := url.Parse(uri)
	if err != nil {
		return Server{}, fmt.Errorf("malformed URI: %w", err)
	}

	uuid := ""
	if u.User != nil {
		uuid = u.User.Username()
	}
	host := u.Hostname()
	port := 443
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Server{}, fmt.Errorf("invalid port: %s", p)
		}
		port = n
	}
	if uuid == "" || host == "" {
		return Server{}, fmt.Errorf("missing uuid or host in: %s", truncate(uri, 80))
	}

	// u.Fragment is already URL-decoded by net/url.
	fragment := u.Fragment

	// Collapse single-value query lists into scalars; drop empties.
	params := make(map[string]string)
	for k, vs := range u.Query() {
		if len(vs) > 0 && vs[0] != "" {
			params[k] = vs[0]
		}
	}

	id := fmt.Sprintf("%s:%d", host, port)
	name := id
	if fragment != "" {
		name = fragment
	}

	return Server{
		ID:      id,
		Name:    name,
		Country: DetectCountry(fragment),
		Host:    host,
		Port:    port,
		UUID:    uuid,
		Params:  params,
	}, nil
}

// ParseSubscription parses a subscription body (raw bytes; base64 or
// plaintext auto-detected) into a deduplicated list of Servers.
func ParseSubscription(body []byte) ([]Server, error) {
	text, err := decodeSubscriptionBody(body)
	if err != nil {
		return nil, err
	}

	var servers []Server
	seen := make(map[string]struct{})
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "vless://") {
			continue
		}
		srv, err := ParseLink(line)
		if err != nil {
			// Providers occasionally include comments or future-format
			// entries; skip malformed silently.
			continue
		}
		if _, dup := seen[srv.ID]; dup {
			continue
		}
		seen[srv.ID] = struct{}{}
		servers = append(servers, srv)
	}
	if servers == nil {
		servers = []Server{}
	}
	return servers, nil
}

func decodeSubscriptionBody(body []byte) (string, error) {
	text := strings.TrimSpace(string(body))
	if strings.Contains(text, "vless://") {
		return text, nil
	}
	// Try base64. Strip whitespace, add missing padding, accept both
	// standard and URL-safe variants.
	compact := whitespaceAll.ReplaceAllString(text, "")
	if compact == "" {
		return "", errors.New("subscription body is neither plain vless:// list nor a base64-encoded one")
	}
	padded := compact + strings.Repeat("=", (4-len(compact)%4)%4)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		decoded, err := enc.DecodeString(padded)
		if err == nil && strings.Contains(string(decoded), "vless://") {
			return string(decoded), nil
		}
	}
	return "", errors.New("subscription body is neither plain vless:// list nor a base64-encoded one")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
