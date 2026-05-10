package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// validOutboundTags is the set of xray outbound tags we expect to find in
// user-supplied routing rules. xray itself accepts arbitrary tag names,
// but yonder only constructs three outbounds (proxy / direct / block),
// so a rule referencing any other tag would silently fall back to the
// first outbound. We reject those at validation time.
var validOutboundTags = map[string]struct{}{
	"direct": {},
	"proxy":  {},
	"block":  {},
}

// ruleMatchFields lists the xray rule fields that count as a "match".
// A rule with none of these matches nothing, which is almost always a
// user mistake worth surfacing before the proxy restarts and the rule
// silently does nothing useful.
var ruleMatchFields = []string{
	"domain", "ip", "port", "network", "source", "user",
	"inboundTag", "protocol", "attrs",
}

// parseXrayRules validates that raw is a usable xray routing-rules document.
//
// Three top-level shapes are accepted for convenience:
//
//  1. {"routing": {"rules": [...], "domainStrategy": "..."}} —
//     the exact shape XKeen ships in /opt/etc/xray/configs/05_routing.json.
//  2. {"rules": [...]} — same minus the wrapping `routing` key.
//  3. [...] — bare rules array.
//
// All three normalise to a flat list of rule objects, returned as
// []json.RawMessage so we can drop them straight back into 05_routing.json
// without re-marshalling each rule. Each rule must have outboundTag in
// {direct, proxy, block} and at least one match field.
func parseXrayRules(raw []byte) ([]json.RawMessage, error) {
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}

	var rulesRaw []any
	switch v := top.(type) {
	case []any:
		rulesRaw = v
	case map[string]any:
		if routing, ok := v["routing"].(map[string]any); ok {
			arr, _ := routing["rules"].([]any)
			rulesRaw = arr
		} else if arr, ok := v["rules"].([]any); ok {
			rulesRaw = arr
		} else {
			return nil, errors.New(`expected {"routing": {"rules": [...]}} or {"rules": [...]} or a bare [...] array`)
		}
	default:
		return nil, errors.New("expected JSON object or array at the top level")
	}

	if len(rulesRaw) == 0 {
		return nil, errors.New("`rules` is empty")
	}

	out := make([]json.RawMessage, 0, len(rulesRaw))
	for i, ri := range rulesRaw {
		rule, ok := ri.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("rule[%d] is not an object", i)
		}
		tagAny, hasTag := rule["outboundTag"]
		tag, _ := tagAny.(string)
		if !hasTag {
			return nil, fmt.Errorf("rule[%d].outboundTag is missing", i)
		}
		if _, ok := validOutboundTags[tag]; !ok {
			return nil, fmt.Errorf("rule[%d].outboundTag must be one of %s; got %q",
				i, sortedKeys(validOutboundTags), tag)
		}
		hasMatch := false
		for _, f := range ruleMatchFields {
			if v, ok := rule[f]; ok && !isEmpty(v) {
				hasMatch = true
				break
			}
		}
		if !hasMatch {
			return nil, fmt.Errorf("rule[%d] has no match field — need at least one of %v…",
				i, ruleMatchFields[:4])
		}
		// xray expects type=field; some users omit it. Normalise.
		if _, hasType := rule["type"]; !hasType {
			rule["type"] = "field"
		}
		blob, err := json.Marshal(rule)
		if err != nil {
			return nil, fmt.Errorf("rule[%d] re-marshal: %v", i, err)
		}
		out = append(out, blob)
	}
	return out, nil
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
