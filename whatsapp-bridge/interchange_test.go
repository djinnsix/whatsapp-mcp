package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

func buildTestConfig(t *testing.T) *BridgeConfig {
	t.Helper()
	data, err := os.ReadFile("../bridge-config.example.json")
	if err != nil {
		t.Fatalf("failed to read example config: %v", err)
	}
	var cfg BridgeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("example config does not parse: %v", err)
	}
	for i := range cfg.SendFilter.Patterns {
		p := &cfg.SendFilter.Patterns[i]
		expr := p.Pattern
		if p.CaseInsensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			t.Fatalf("example pattern %q does not compile: %v", p.Pattern, err)
		}
		p.re = re
	}
	return &cfg
}

func TestCheckOutbound(t *testing.T) {
	cfg := buildTestConfig(t)
	filtered := cfg.SendFilter.JIDs[0]

	cases := []struct {
		name    string
		jid     string
		message string
		media   bool
		blocked bool
	}{
		{"clean message passes", filtered, "[Wren] Opened PR #42 for KAN-1445.", false, false},
		{"aws key blocked", filtered, "creds are AKIAIOSFODNN7EXAMPLE ok", false, true},
		{"private key blocked", filtered, "-----BEGIN RSA PRIVATE KEY-----", false, true},
		{"credential assignment blocked case-insensitively", filtered, "Password: hunter2hunter2", false, true},
		{"customer name blocked case-insensitively", filtered, "spoke to Customer-One today", false, true},
		{"account id blocked", filtered, "deployed to 111111111111 just now", false, true},
		{"media refused to filtered chat", filtered, "here's a screenshot", true, true},
		{"unfiltered jid passes everything", "other@g.us", "AKIAIOSFODNN7EXAMPLE", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := cfg.CheckOutbound(tc.jid, tc.message, tc.media)
			if tc.blocked && reason == "" {
				t.Errorf("expected block, got pass")
			}
			if !tc.blocked && reason != "" {
				t.Errorf("expected pass, got blocked: %s", reason)
			}
		})
	}
}

func TestMissingConfigDisablesFeatures(t *testing.T) {
	cfg := &BridgeConfig{}
	if reason := cfg.CheckOutbound("any@g.us", "AKIAIOSFODNN7EXAMPLE", false); reason != "" {
		t.Errorf("nil filter should pass everything, got: %s", reason)
	}
	cfg.FireHooks(HookPayload{ChatJID: "any@g.us"}, nil) // must not panic with no hooks
}
