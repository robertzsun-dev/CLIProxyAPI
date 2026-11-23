package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestNativeModelFamilies(t *testing.T) {
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{Client: config.ClientConfig{NativeModelsOnly: true, OpenCodeLocalModels: []string{"deepseek-v4.1-flash-1017"}}}}
	for _, tt := range []struct {
		protocol, ua, model string
		allowed             bool
	}{
		{"claude", "", "claude-fable-5-2", true},
		{"claude", "", "claude-fable-5-dd-anul-6.5-tpg", false},
		{"claude", "", "gpt-6.2-astra", false},
		{"gemini", "", "models/gemini-4-neon", true},
		{"gemini", "", "claude-sonnet-5-5", false},
		{"openai-response", "codex_exec/1", "gpt-6.2-sol(high)", true},
		{"openai-response", "codex_exec/1", "muse-spark-1.3", false},
		{"openai-response", "codex-tui/1", "gpt-oss-120b", false},
		{"openai", "muse-code/1", "muse-wonder-1.0", true},
		{"openai", "muse-code/1", "gpt-6.2-sol", false},
		{"openai", "OpenCode", "deepseek-v4.1-flash-1017", true},
		{"openai", "OpenCode", "gemini-4-neon", false},
		{"openai-response", "other-client", "claude-opus-5-5", true},
	} {
		t.Run(tt.protocol+"/"+tt.model, func(t *testing.T) {
			headers := http.Header{"User-Agent": []string{tt.ua}}
			errMsg := h.NativeModelError(tt.protocol, headers, tt.model)
			if (errMsg == nil) != tt.allowed {
				t.Fatalf("allowed=%v, err=%v", tt.allowed, errMsg)
			}
		})
	}
	if errMsg := h.nativeRouteError(context.Background(), "claude", "claude-opus-5-5", modelExecutionOptions{}, modelRouteDecision{Model: "gpt-6.2-sol"}); errMsg == nil {
		t.Fatal("accepted cross-family route override")
	}
	h.Cfg.Client.NativeModelsOnly = false
	if errMsg := h.NativeModelError("claude", nil, "gpt-6.2-sol"); errMsg != nil {
		t.Fatal("default compatibility changed", errMsg)
	}
}

func TestNativeModelFamiliesStreamingAndCounting(t *testing.T) {
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{Client: config.ClientConfig{NativeModelsOnly: true}}}
	_, _, streamErr := h.ExecuteStreamWithAuthManager(context.Background(), "claude", "gpt-6.2-sol", nil, "")
	if errMsg := <-streamErr; errMsg == nil || errMsg.StatusCode != 400 {
		t.Fatalf("stream error=%v", errMsg)
	}
	_, _, errMsg := h.ExecuteCountWithAuthManager(context.Background(), "claude", "gpt-6.2-sol", nil, "")
	if errMsg == nil || errMsg.StatusCode != 400 {
		t.Fatalf("count error=%v", errMsg)
	}
}
