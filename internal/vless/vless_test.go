package vless

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestDetectCountry(t *testing.T) {
	cases := []struct{ in, want string }{
		// Flag emoji
		{"\U0001F1F5\U0001F1F1 Poland", "PL"},
		{"\U0001F1E9\U0001F1EA", "DE"},
		{"\U0001F1FA\U0001F1F8 USA", "US"},
		// Native-language names
		{"Польша", "PL"},
		{"Германия", "DE"},
		{"Финляндия", "FI"},
		// English names
		{"Germany", "DE"},
		{"united states", "US"},
		// Decoration stripped
		{"⚡Польша", "PL"},
		{"(Germany)", "DE"},
		// Unknown
		{"Atlantis", "??"},
		{"", "??"},
	}
	for _, c := range cases {
		if got := DetectCountry(c.in); got != c.want {
			t.Errorf("DetectCountry(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseLink_BasicReality(t *testing.T) {
	uri := "vless://5ce044d1-6a0b-4dc5-b2c9-6eb296642a1c@example.com:8443" +
		"?security=reality&type=tcp&flow=xtls-rprx-vision&sni=test.example" +
		"&fp=chrome&pbk=KEY&sid=SID#%F0%9F%87%B5%F0%9F%87%B1Poland"
	srv, err := ParseLink(uri)
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	if srv.Host != "example.com" {
		t.Errorf("host = %q, want example.com", srv.Host)
	}
	if srv.Port != 8443 {
		t.Errorf("port = %d, want 8443", srv.Port)
	}
	if srv.UUID != "5ce044d1-6a0b-4dc5-b2c9-6eb296642a1c" {
		t.Errorf("uuid = %q", srv.UUID)
	}
	if srv.ID != "example.com:8443" {
		t.Errorf("id = %q", srv.ID)
	}
	if srv.Country != "PL" {
		t.Errorf("country = %q, want PL", srv.Country)
	}
	if srv.Params["security"] != "reality" {
		t.Errorf("params[security] = %q", srv.Params["security"])
	}
	if srv.Params["pbk"] != "KEY" {
		t.Errorf("params[pbk] = %q", srv.Params["pbk"])
	}
}

func TestParseLink_Errors(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{"missing uuid", "vless://@example.com:8443"},
		{"missing host", "vless://uuid@:8443"},
		{"wrong scheme", "vmess://uuid@example.com:8443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseLink(c.uri); err == nil {
				t.Errorf("ParseLink(%q): expected error, got nil", c.uri)
			}
		})
	}
}

func TestParseLink_DefaultPort(t *testing.T) {
	srv, err := ParseLink("vless://uuid-x@example.com")
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	if srv.Port != 443 {
		t.Errorf("port = %d, want 443", srv.Port)
	}
}

func TestParseLink_NoFragmentUsesIDAsName(t *testing.T) {
	srv, err := ParseLink("vless://uuid-x@example.com:1234")
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	if srv.Name != "example.com:1234" {
		t.Errorf("name = %q", srv.Name)
	}
}

var subscriptionURIs = []string{
	"vless://aaa@host1.com:443?security=reality#%F0%9F%87%B5%F0%9F%87%B1Poland",
	"vless://bbb@host2.com:8443?security=reality#%F0%9F%87%A9%F0%9F%87%AAGermany",
}

func TestParseSubscription_Plaintext(t *testing.T) {
	body := []byte(strings.Join(subscriptionURIs, "\n"))
	servers, err := ParseSubscription(body)
	if err != nil {
		t.Fatalf("ParseSubscription: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(servers))
	}
	if servers[0].Country != "PL" || servers[1].Country != "DE" {
		t.Errorf("countries = %q, %q", servers[0].Country, servers[1].Country)
	}
}

func TestParseSubscription_Base64(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(strings.Join(subscriptionURIs, "\n")))
	servers, err := ParseSubscription([]byte(encoded))
	if err != nil {
		t.Fatalf("ParseSubscription: %v", err)
	}
	if len(servers) != 2 {
		t.Errorf("got %d servers, want 2", len(servers))
	}
}

func TestParseSubscription_Base64NoPadding(t *testing.T) {
	encoded := strings.TrimRight(
		base64.StdEncoding.EncodeToString([]byte(strings.Join(subscriptionURIs, "\n"))),
		"=",
	)
	servers, err := ParseSubscription([]byte(encoded))
	if err != nil {
		t.Fatalf("ParseSubscription: %v", err)
	}
	if len(servers) != 2 {
		t.Errorf("got %d servers, want 2", len(servers))
	}
}

func TestParseSubscription_DedupByHostPort(t *testing.T) {
	body := []byte(subscriptionURIs[0] + "\n" + subscriptionURIs[0])
	servers, _ := ParseSubscription(body)
	if len(servers) != 1 {
		t.Errorf("got %d servers, want 1", len(servers))
	}
}

func TestParseSubscription_SkipsMalformedLines(t *testing.T) {
	body := []byte(subscriptionURIs[0] + "\nvless://broken\nplain comment line\n" +
		subscriptionURIs[1])
	servers, _ := ParseSubscription(body)
	if len(servers) != 2 {
		t.Errorf("got %d servers, want 2", len(servers))
	}
}

func TestParseSubscription_InvalidBodyRaises(t *testing.T) {
	if _, err := ParseSubscription([]byte("not base64 nor a vless list")); err == nil {
		t.Errorf("expected error for unparseable body")
	}
}

func TestParseSubscription_EmptyBodyRaises(t *testing.T) {
	if _, err := ParseSubscription([]byte("")); err == nil {
		t.Errorf("expected error for empty body")
	}
}
