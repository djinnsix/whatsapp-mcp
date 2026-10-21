package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// Bridge extensions for the agent-interchange group: on-receive hooks and a
// server-side outbound send filter. Config lives in the store directory
// (gitignored) because filter patterns include values that must never be
// committed. Override the path with WHATSAPP_BRIDGE_CONFIG.

const defaultBridgeConfigPath = "store/bridge-config.json"

// HookConfig delivers inbound messages for the listed JIDs to a webhook, a
// command, or both. An empty JID list matches nothing; watching must be
// explicit.
type HookConfig struct {
	JIDs       []string `json:"jids"`
	WebhookURL string   `json:"webhook_url,omitempty"`
	Command    []string `json:"command,omitempty"`
}

type FilterPattern struct {
	Pattern         string `json:"pattern"`
	Description     string `json:"description"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`

	re *regexp.Regexp
}

// SendFilterConfig refuses outbound sends to the listed JIDs when the message
// text matches any pattern. Media sends are refused too unless allow_media is
// set, because file contents cannot be scanned.
type SendFilterConfig struct {
	JIDs       []string        `json:"jids"`
	Patterns   []FilterPattern `json:"patterns"`
	AllowMedia bool            `json:"allow_media,omitempty"`
}

type BridgeConfig struct {
	Hooks      []HookConfig      `json:"hooks,omitempty"`
	SendFilter *SendFilterConfig `json:"send_filter,omitempty"`
}

// LoadBridgeConfig reads and validates the bridge config. A missing file
// disables both features; an unreadable file or a non-compiling filter
// pattern is a hard error, because a filter that silently fails to load
// would defeat its purpose.
func LoadBridgeConfig(logger waLog.Logger) (*BridgeConfig, error) {
	path := os.Getenv("WHATSAPP_BRIDGE_CONFIG")
	if path == "" {
		path = defaultBridgeConfigPath
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Infof("No bridge config at %s; hooks and send filter disabled", path)
			return &BridgeConfig{}, nil
		}
		return nil, fmt.Errorf("failed to read bridge config %s: %v", path, err)
	}

	var cfg BridgeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid bridge config %s: %v", path, err)
	}

	if cfg.SendFilter != nil {
		for i := range cfg.SendFilter.Patterns {
			p := &cfg.SendFilter.Patterns[i]
			expr := p.Pattern
			if p.CaseInsensitive {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("send filter pattern %q does not compile: %v", p.Pattern, err)
			}
			p.re = re
		}
		logger.Infof("Send filter active for %d chat(s) with %d pattern(s)",
			len(cfg.SendFilter.JIDs), len(cfg.SendFilter.Patterns))
	}
	if len(cfg.Hooks) > 0 {
		logger.Infof("On-receive hooks configured: %d", len(cfg.Hooks))
	}

	return &cfg, nil
}

func containsJID(list []string, jid string) bool {
	for _, j := range list {
		if j == jid {
			return true
		}
	}
	return false
}

// CheckOutbound returns a human-readable refusal reason when the message must
// not be sent to this recipient, or an empty string when the send may proceed.
func (cfg *BridgeConfig) CheckOutbound(recipientJID, message string, hasMedia bool) string {
	f := cfg.SendFilter
	if f == nil || !containsJID(f.JIDs, recipientJID) {
		return ""
	}
	if hasMedia && !f.AllowMedia {
		return "media sends are not allowed to filtered chats (file contents cannot be scanned)"
	}
	for i := range f.Patterns {
		if f.Patterns[i].re.MatchString(message) {
			return "blocked by send filter: " + f.Patterns[i].Description
		}
	}
	return ""
}

// HookPayload is what a webhook or command receives for each new message.
// Timestamp is RFC 3339 in UTC.
type HookPayload struct {
	ID        string `json:"id"`
	ChatJID   string `json:"chat_jid"`
	Sender    string `json:"sender"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	IsFromMe  bool   `json:"is_from_me"`
	MediaType string `json:"media_type,omitempty"`
	Filename  string `json:"filename,omitempty"`
	// EditOf is set when this message is an edit: the ID of the message it edits (KAN-2066).
	EditOf string `json:"edit_of,omitempty"`
}

var hookHTTPClient = &http.Client{Timeout: 5 * time.Second}

// FireHooks delivers the payload to every hook watching its chat JID.
// Fire-and-forget: each delivery runs in its own goroutine with a short
// timeout so a slow or dead hook can never block message handling.
func (cfg *BridgeConfig) FireHooks(payload HookPayload, logger waLog.Logger) {
	for i := range cfg.Hooks {
		hook := cfg.Hooks[i]
		if !containsJID(hook.JIDs, payload.ChatJID) {
			continue
		}
		go runHook(hook, payload, logger)
	}
}

func runHook(hook HookConfig, payload HookPayload, logger waLog.Logger) {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf("Hook payload marshal failed: %v", err)
		return
	}

	if hook.WebhookURL != "" {
		resp, err := hookHTTPClient.Post(hook.WebhookURL, "application/json", bytes.NewReader(body))
		if err != nil {
			logger.Warnf("Hook webhook %s failed: %v", hook.WebhookURL, err)
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				logger.Warnf("Hook webhook %s returned %d", hook.WebhookURL, resp.StatusCode)
			}
		}
	}

	if len(hook.Command) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, hook.Command[0], hook.Command[1:]...)
		cmd.Stdin = bytes.NewReader(body)
		if out, err := cmd.CombinedOutput(); err != nil {
			logger.Warnf("Hook command %s failed: %v (%s)", hook.Command[0], err, string(out))
		}
	}
}
