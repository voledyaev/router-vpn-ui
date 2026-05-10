package main

import (
	"strings"
	"testing"
)

func TestParseXrayRules_FullRoutingShape(t *testing.T) {
	rules, err := parseXrayRules([]byte(
		`{"routing": {"rules": [{"outboundTag": "direct", "ip": ["10.0.0.0/8"]}]}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	// type=field auto-filled by parser
	if !strings.Contains(string(rules[0]), `"type":"field"`) {
		t.Errorf("type=field not added:\n%s", rules[0])
	}
}

func TestParseXrayRules_RulesOnlyShape(t *testing.T) {
	rules, err := parseXrayRules([]byte(
		`{"rules": [{"outboundTag": "proxy", "domain": ["example.com"]}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("got %d rules, want 1", len(rules))
	}
}

func TestParseXrayRules_BareArrayShape(t *testing.T) {
	rules, err := parseXrayRules([]byte(
		`[{"outboundTag": "block", "ip": ["1.1.1.1"]}]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("got %d rules, want 1", len(rules))
	}
}

func TestParseXrayRules_RejectsInvalidJSON(t *testing.T) {
	_, err := parseXrayRules([]byte(`{ not json`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("err = %v", err)
	}
}

func TestParseXrayRules_RejectsUnknownTopLevelKey(t *testing.T) {
	_, err := parseXrayRules([]byte(`{"foo": "bar"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "rules") {
		t.Errorf("err = %v", err)
	}
}

func TestParseXrayRules_RejectsTopLevelString(t *testing.T) {
	_, err := parseXrayRules([]byte(`"just a string"`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "object or array") {
		t.Errorf("err = %v", err)
	}
}

func TestParseXrayRules_RejectsEmptyRulesArray(t *testing.T) {
	_, err := parseXrayRules([]byte(`{"rules": []}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v", err)
	}
}

func TestParseXrayRules_RejectsInvalidOutboundTag(t *testing.T) {
	_, err := parseXrayRules([]byte(`[{"outboundTag": "internet", "ip": ["1.1.1.1"]}]`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "outboundTag") {
		t.Errorf("err = %v", err)
	}
}

func TestParseXrayRules_RejectsNoMatchField(t *testing.T) {
	_, err := parseXrayRules([]byte(`[{"outboundTag": "direct"}]`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "match field") {
		t.Errorf("err = %v", err)
	}
}

func TestParseXrayRules_PreservesExistingType(t *testing.T) {
	rules, err := parseXrayRules([]byte(
		`[{"outboundTag": "direct", "ip": ["1.1.1.1"], "type": "custom"}]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(rules[0]), `"type":"custom"`) {
		t.Errorf("type=custom should be preserved:\n%s", rules[0])
	}
}

func TestParseXrayRules_MultipleValidRules(t *testing.T) {
	rules, err := parseXrayRules([]byte(`[
		{"outboundTag": "direct", "ip": ["10.0.0.0/8"]},
		{"outboundTag": "proxy", "domain": ["example.com"]},
		{"outboundTag": "block", "domain": ["ads.example.com"]}
	]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 3 {
		t.Errorf("got %d rules, want 3", len(rules))
	}
	for i, r := range rules {
		if !strings.Contains(string(r), `"type":"field"`) {
			t.Errorf("rule[%d] missing type=field", i)
		}
	}
}

func TestParseXrayRules_RejectsRuleNotAnObject(t *testing.T) {
	_, err := parseXrayRules([]byte(
		`[{"outboundTag": "direct", "ip": ["1.1.1.1"]}, "string-rule"]`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not an object") {
		t.Errorf("err = %v", err)
	}
}

func TestParseXrayRules_AllMatchFieldsAccepted(t *testing.T) {
	for _, field := range []string{"domain", "ip", "port", "network",
		"source", "user", "inboundTag", "protocol"} {
		t.Run(field, func(t *testing.T) {
			text := `[{"outboundTag": "direct", "` + field + `": ["x"]}]`
			if _, err := parseXrayRules([]byte(text)); err != nil {
				t.Errorf("%s should be accepted: %v", field, err)
			}
		})
	}
}
