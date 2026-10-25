package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const codexUsageLimitEvent = `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":3600}}`

// A usage limit that arrives behind held handshake frames has not reached the
// client yet, so the attempt fails and the conductor can use another account.
func TestCodexExecutor_BootstrapBuffering_UsageLimitFailsAttempt(t *testing.T) {
	server := codexSSEServer(codexCreatedEvent, codexInProgressEvent, `{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":3600}}`)
	defer server.Close()

	req, opts := codexTestRequest()
	result, err := NewCodexExecutor(codexBufferingConfig(true)).ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
	if err == nil || result != nil {
		t.Fatalf("usage limit must fail the attempt before the stream is committed: result=%v err=%v", result, err)
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	retry, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retry.RetryAfter() == nil || *retry.RetryAfter() != time.Hour {
		t.Fatalf("one-hour retry-after lost: %v", err)
	}
}

// A later create refused for quota on a duplex socket, with nothing of it sent
// to the client, is handed back for replay instead of forwarded as an error.
func TestCodexDuplexQuotaRejectionHandsRequestBack(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
		if _, _, err = c.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`))
		if _, _, err = c.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		_ = c.WriteMessage(websocket.TextMessage, []byte(codexUsageLimitEvent))
		_, _, _ = c.ReadMessage()
	}))
	defer upstream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	input := make(chan core.WebsocketInput, 1)
	ctx = core.WithWebsocketInput(core.WithDownstreamWebsocket(ctx), input)
	cfg := &config.Config{}
	cfg.Codex.ResponseSteering = true
	cfg.CodexResponseSteering = true
	executor := NewCodexWebsocketsExecutor(cfg)
	executor.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	manager := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(3, 30*time.Second, 0)
	manager.RegisterExecutor(executor)
	model := "quota-handback-model"
	bad := &auth.Auth{ID: t.Name() + "-bad", Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "test", "base_url": upstream.URL, "websockets": "true", "priority": "4"}}
	good := &auth.Auth{ID: t.Name() + "-good", Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "unused", "base_url": upstream.URL, "websockets": "true", "priority": "3"}}
	for _, candidate := range []*auth.Auth{bad, good} {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: model}})
		id := candidate.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := manager.Register(ctx, candidate); err != nil {
			t.Fatal(err)
		}
	}
	req := core.Request{Model: model, Payload: []byte(`{"model":"quota-handback-model","input":[]}`)}
	opts := core.Options{SourceFormat: translator.FromString("codex"), Metadata: map[string]any{core.ExecutionSessionMetadataKey: t.Name()}}
	result, err := manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	second := `{"type":"response.create","input":[{"type":"message","role":"user","content":"two"}]}`
	var failover *core.WebsocketCredentialFailoverError
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			var ok bool
			if failover, ok = core.AsWebsocketCredentialFailover(chunk.Err); !ok {
				t.Fatalf("terminal error is not a failover: %v", chunk.Err)
			}
			continue
		}
		event := gjson.GetBytes(chunk.Payload, "type").String()
		if event == "error" || strings.Contains(string(chunk.Payload), "usage_limit_reached") {
			t.Fatalf("quota error forwarded to client: %s", chunk.Payload)
		}
		if event == "response.completed" {
			input <- core.WebsocketInput{Payload: []byte(second)}
		}
	}
	if failover == nil {
		t.Fatal("no failover signal")
	}
	if len(failover.Frames) != 1 || string(failover.Frames[0]) != second {
		t.Fatalf("handed-back frames = %q", failover.Frames)
	}
	if gjson.GetBytes(failover.ErrorPayload, "error.type").String() != "usage_limit_reached" {
		t.Fatalf("original error payload lost: %s", failover.ErrorPayload)
	}
	current, _ := manager.GetByID(bad.ID)
	if current.Quota.Reason != "credential_quota" || !current.Quota.NextRecoverAt.After(time.Now().Add(30*time.Minute)) {
		t.Fatalf("exhausted credential not cooled: %+v", current.Quota)
	}
	if healthy, _ := manager.GetByID(good.ID); healthy.Unavailable || healthy.LastError != nil {
		t.Fatal("healthy account changed")
	}
	if attempts.Load() != 1 {
		t.Fatalf("executor replayed on its own (%d upstream sockets)", attempts.Load())
	}
}
