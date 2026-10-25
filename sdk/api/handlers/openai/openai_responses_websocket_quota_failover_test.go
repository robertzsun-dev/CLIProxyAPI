package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const quotaFailoverUsageLimitEvent = `{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"pro","resets_in_seconds":3600}}`

// quotaFailoverUpstream serves two Codex accounts on one websocket endpoint.
// The exhausted account answers its first create and refuses every later one
// with usage_limit_reached; the healthy account answers everything.
type quotaFailoverUpstream struct {
	mu       sync.Mutex
	received map[string][][]byte
	server   *httptest.Server
}

func newQuotaFailoverUpstream(t *testing.T) *quotaFailoverUpstream {
	t.Helper()
	u := &quotaFailoverUpstream{received: make(map[string][][]byte)}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = c.Close() }()
		for n := 1; ; n++ {
			_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
			_, payload, errRead := c.ReadMessage()
			if errRead != nil {
				return
			}
			u.mu.Lock()
			u.received[account] = append(u.received[account], payload)
			total := len(u.received[account])
			u.mu.Unlock()
			if account == "exhausted" && n > 1 {
				_ = c.WriteMessage(websocket.TextMessage, []byte(quotaFailoverUsageLimitEvent))
				continue
			}
			id := fmt.Sprintf("%s-%d", account, total)
			_ = c.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id)))
			_ = c.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}`, id)))
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *quotaFailoverUpstream) frames(account string) [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.received[account]...)
}

func startQuotaFailoverProxy(t *testing.T, upstreamURL string, model string, withHealthy bool) (*websocket.Conn, *coreauth.Manager) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Codex.ResponseSteering = true
	cfg.CodexResponseSteering = true
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(3, 30*time.Second, 0)
	manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
	accounts := []*coreauth.Auth{{
		ID: t.Name() + "-exhausted", Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "exhausted", "base_url": upstreamURL, "websockets": "true", "priority": "9"},
	}}
	if withHealthy {
		accounts = append(accounts, &coreauth.Auth{
			ID: t.Name() + "-healthy", Provider: "codex", Status: coreauth.StatusActive,
			Attributes: map[string]string{"api_key": "healthy", "base_url": upstreamURL, "websockets": "true", "priority": "1"},
		})
	}
	for _, account := range accounts {
		registry.GetGlobalRegistry().RegisterClient(account.ID, "codex", []*registry.ModelInfo{{ID: model}})
		id := account.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), account); err != nil {
			t.Fatal(err)
		}
	}
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)
	downstream := httptest.NewServer(router)
	t.Cleanup(downstream.Close)
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, manager
}

// readUntilTerminal returns the events of one response, ending at its
// completion or at an error event.
func readUntilTerminal(t *testing.T, c *websocket.Conn) []string {
	t.Helper()
	var events []string
	for {
		_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
		_, payload, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read after %v: %v", events, err)
		}
		events = append(events, string(payload))
		switch gjson.GetBytes(payload, "type").String() {
		case "response.completed", "error":
			return events
		}
	}
}

func sendQuotaFailoverFrame(t *testing.T, c *websocket.Conn, frame string) {
	t.Helper()
	if err := c.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatal(err)
	}
}

func assertCredentialQuotaCooling(t *testing.T, manager *coreauth.Manager, authID string) {
	t.Helper()
	current, ok := manager.GetByID(authID)
	if !ok {
		t.Fatalf("auth %s missing", authID)
	}
	if current.Quota.Reason != "credential_quota" || !current.Quota.NextRecoverAt.After(time.Now().Add(30*time.Minute)) {
		t.Fatalf("exhausted credential not cooled for quota: %+v", current.Quota)
	}
}

func TestResponsesWebsocketQuotaFailoverReplaysFullCreate(t *testing.T) {
	model := "quota-failover-full-model"
	upstream := newQuotaFailoverUpstream(t)
	c, manager := startQuotaFailoverProxy(t, upstream.server.URL, model, true)

	sendQuotaFailoverFrame(t, c, `{"type":"response.create","model":"`+model+`","input":[{"type":"message","role":"user","content":"one"}]}`)
	if events := readUntilTerminal(t, c); gjson.Get(events[len(events)-1], "response.id").String() != "exhausted-1" {
		t.Fatalf("first turn not served by bound account: %v", events)
	}

	second := `{"type":"response.create","model":"` + model + `","input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"assistant","content":"ok"},{"type":"message","role":"user","content":"two"}]}`
	sendQuotaFailoverFrame(t, c, second)
	events := readUntilTerminal(t, c)
	for _, event := range events {
		if gjson.Get(event, "type").String() == "error" {
			t.Fatalf("quota error reached the client although another account was available: %v", events)
		}
	}
	if got := gjson.Get(events[len(events)-1], "response.id").String(); got != "healthy-1" {
		t.Fatalf("replayed request not served by healthy account: %v", events)
	}
	healthyFrames := upstream.frames("healthy")
	if len(healthyFrames) != 1 {
		t.Fatalf("healthy account received %d frames", len(healthyFrames))
	}
	if got := gjson.GetBytes(healthyFrames[0], "input.#").Int(); got != 3 {
		t.Fatalf("replay did not carry the full input: %s", healthyFrames[0])
	}
	if gjson.GetBytes(healthyFrames[0], "previous_response_id").Exists() {
		t.Fatalf("replay kept previous_response_id: %s", healthyFrames[0])
	}
	assertCredentialQuotaCooling(t, manager, t.Name()+"-exhausted")

	// The session stays on the healthy account afterwards.
	sendQuotaFailoverFrame(t, c, `{"type":"response.create","model":"`+model+`","previous_response_id":"healthy-1","input":[{"type":"message","role":"user","content":"three"}]}`)
	events = readUntilTerminal(t, c)
	if got := gjson.Get(events[len(events)-1], "response.id").String(); got != "healthy-2" {
		t.Fatalf("follow-up not served by healthy account: %v", events)
	}
	if n := len(upstream.frames("exhausted")); n != 2 {
		t.Fatalf("exhausted account received %d frames, want 2", n)
	}
}

func TestResponsesWebsocketQuotaFailoverAsksForFullInputAfterPreviousResponse(t *testing.T) {
	model := "quota-failover-incremental-model"
	upstream := newQuotaFailoverUpstream(t)
	c, manager := startQuotaFailoverProxy(t, upstream.server.URL, model, true)

	sendQuotaFailoverFrame(t, c, `{"type":"response.create","model":"`+model+`","input":[{"type":"message","role":"user","content":"one"}]}`)
	readUntilTerminal(t, c)

	sendQuotaFailoverFrame(t, c, `{"type":"response.create","model":"`+model+`","previous_response_id":"exhausted-1","input":[{"type":"message","role":"user","content":"two"}]}`)
	events := readUntilTerminal(t, c)
	last := events[len(events)-1]
	if gjson.Get(last, "type").String() != "error" || gjson.Get(last, "error.code").String() != "previous_response_not_found" {
		t.Fatalf("incremental request should ask for full input, got %v", events)
	}
	if strings.Contains(strings.Join(events, "\n"), "usage_limit_reached") {
		t.Fatalf("quota error leaked to client: %v", events)
	}
	if n := len(upstream.frames("healthy")); n != 0 {
		t.Fatalf("incremental request was replayed on another account (%d frames)", n)
	}
	assertCredentialQuotaCooling(t, manager, t.Name()+"-exhausted")

	// Codex resends the whole conversation on the same socket.
	sendQuotaFailoverFrame(t, c, `{"type":"response.create","model":"`+model+`","input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"assistant","content":"ok"},{"type":"message","role":"user","content":"two"}]}`)
	events = readUntilTerminal(t, c)
	if got := gjson.Get(events[len(events)-1], "response.id").String(); got != "healthy-1" {
		t.Fatalf("full resend not served by healthy account: %v", events)
	}
}

func TestResponsesWebsocketQuotaFailoverWithoutOtherAccountReturnsQuotaError(t *testing.T) {
	model := "quota-failover-single-model"
	upstream := newQuotaFailoverUpstream(t)
	c, manager := startQuotaFailoverProxy(t, upstream.server.URL, model, false)

	sendQuotaFailoverFrame(t, c, `{"type":"response.create","model":"`+model+`","input":[{"type":"message","role":"user","content":"one"}]}`)
	readUntilTerminal(t, c)

	sendQuotaFailoverFrame(t, c, `{"type":"response.create","model":"`+model+`","input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"user","content":"two"}]}`)
	events := readUntilTerminal(t, c)
	last := events[len(events)-1]
	if gjson.Get(last, "type").String() != "error" || gjson.Get(last, "status").Int() != http.StatusTooManyRequests ||
		gjson.Get(last, "error.type").String() != "usage_limit_reached" {
		t.Fatalf("client did not get the usage limit error: %v", events)
	}
	assertCredentialQuotaCooling(t, manager, t.Name()+"-exhausted")
	if n := len(upstream.frames("exhausted")); n != 2 {
		t.Fatalf("exhausted account received %d frames, want 2", n)
	}
}
