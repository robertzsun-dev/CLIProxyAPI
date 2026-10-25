package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const nativeTurnMismatchBody = `{"error":{"message":"native turn auth context mismatch: scopes","type":"internal_error","param":null,"code":null}}`

func TestCodexStatusErrTurnAuthContextMismatchIsPreviousResponseNotFound(t *testing.T) {
	err := newCodexStatusErr(http.StatusInternalServerError, []byte(nativeTurnMismatchBody))
	if err.StatusCode() != http.StatusConflict {
		t.Fatalf("status = %d, want 409", err.StatusCode())
	}
	if code := gjson.Get(err.Error(), "error.code").String(); code != "previous_response_not_found" {
		t.Fatalf("error.code = %q, want previous_response_not_found; body=%s", code, err.Error())
	}
	if typ := gjson.Get(err.Error(), "error.type").String(); typ != "invalid_request_error" {
		t.Fatalf("error.type = %q", typ)
	}
	if !strings.Contains(err.Error(), "native turn auth context mismatch") {
		t.Fatalf("upstream message lost: %s", err.Error())
	}
	if !clienterror.IsRequestFault(err.StatusCode(), err) {
		t.Fatal("mismatch must be a request fault (no cooldown, no rotation)")
	}
}

func TestCodexWebsocketErrorTurnAuthContextMismatchIsPreviousResponseNotFound(t *testing.T) {
	payload := []byte(`{"type":"error","status":500,"error":{"message":"native turn auth context mismatch: scopes","type":"internal_error","param":null,"code":null}}`)
	err, ok := parseCodexWebsocketError(payload)
	if !ok {
		t.Fatal("websocket error not parsed")
	}
	var sc interface{ StatusCode() int }
	if !errors.As(err, &sc) || sc.StatusCode() != http.StatusConflict {
		t.Fatalf("status = %v, want 409", err)
	}
	if !strings.Contains(err.Error(), "previous_response_not_found") {
		t.Fatalf("error not classified: %s", err.Error())
	}
}

func TestCodexTerminalFailureTurnAuthContextMismatch(t *testing.T) {
	event := []byte(`{"type":"response.failed","response":{"error":{"message":"native turn auth context mismatch: scopes","type":"internal_error"}}}`)
	err, _, ok := codexTerminalFailureErr(event)
	if !ok {
		t.Fatal("terminal failure not detected")
	}
	if err.StatusCode() != http.StatusConflict || !strings.Contains(err.Error(), "previous_response_not_found") {
		t.Fatalf("got status=%d body=%s", err.StatusCode(), err.Error())
	}
}

func TestCodexTurnStateRegistry(t *testing.T) {
	codexTurnStates.reset()
	t.Cleanup(codexTurnStates.reset)

	if codexTurnStates.foreign("ts-1", "auth-b") {
		t.Fatal("unknown turn state must be forwarded")
	}
	codexTurnStates.remember("ts-1", "auth-a")
	codexTurnStates.remember("ts-1", "auth-b") // first issuer wins
	if codexTurnStates.foreign("ts-1", "auth-a") {
		t.Fatal("owner must keep its turn state")
	}
	if !codexTurnStates.foreign("ts-1", "auth-b") {
		t.Fatal("turn state of auth-a must not go to auth-b")
	}

	codexTurnStates.reject("ts-2", "auth-c")
	if !codexTurnStates.foreign("ts-2", "auth-c") {
		t.Fatal("rejected turn state must not be resent to the rejecting credential")
	}
	codexTurnStates.remember("ts-2", "auth-c")
	if !codexTurnStates.foreign("ts-2", "auth-c") {
		t.Fatal("a rejecting credential must not become the owner")
	}

	headers := http.Header{}
	headers.Set("X-Codex-Turn-State", "ts-1")
	dropForeignCodexTurnState(headers, "auth-a")
	if headers.Get("X-Codex-Turn-State") != "ts-1" {
		t.Fatal("owner header dropped")
	}
	dropForeignCodexTurnState(headers, "auth-b")
	if headers.Get("X-Codex-Turn-State") != "" {
		t.Fatal("foreign header forwarded")
	}
}

func TestObserveCodexTurnStateEvent(t *testing.T) {
	codexTurnStates.reset()
	t.Cleanup(codexTurnStates.reset)
	observeCodexTurnStateEvent([]byte(`{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"ts-meta"}}`), "auth-a")
	if !codexTurnStates.foreign("ts-meta", "auth-b") {
		t.Fatal("metadata turn state not recorded")
	}
}

func TestApplyCodexHeadersDropsForeignTurnState(t *testing.T) {
	codexTurnStates.reset()
	t.Cleanup(codexTurnStates.reset)
	codexTurnStates.remember("ts-a", "auth-a")
	client := http.Header{}
	client.Set("X-Codex-Turn-State", "ts-a")

	for _, tc := range []struct {
		authID string
		want   string
	}{{"auth-a", "ts-a"}, {"auth-b", ""}} {
		req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
		applyCodexHeaders(req, &cliproxyauth.Auth{ID: tc.authID}, "token", true, nil, client)
		if got := req.Header.Get("X-Codex-Turn-State"); got != tc.want {
			t.Fatalf("http auth=%s turn state = %q, want %q", tc.authID, got, tc.want)
		}
		ws := applyCodexWebsocketHeaders(context.Background(), http.Header{}, &cliproxyauth.Auth{ID: tc.authID}, "token", nil, false, client)
		if got := ws.Get("X-Codex-Turn-State"); got != tc.want {
			t.Fatalf("ws auth=%s turn state = %q, want %q", tc.authID, got, tc.want)
		}
	}
}

func TestDoCodexHTTPWithRecoveryResendsWithoutForeignTurnState(t *testing.T) {
	codexTurnStates.reset()
	t.Cleanup(codexTurnStates.reset)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"input":"x"}` {
			t.Errorf("body not replayed: %q", body)
		}
		state := r.Header.Get("X-Codex-Turn-State")
		mu.Lock()
		seen = append(seen, state)
		mu.Unlock()
		if state != "" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, nativeTurnMismatchBody)
			return
		}
		w.Header().Set("X-Codex-Turn-State", "ts-new")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"input":"x"}`))
	req.Header.Set("X-Codex-Turn-State", "ts-old")
	resp, err := doCodexHTTPWithRecovery(context.Background(), srv.Client(), req, "auth-b")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(seen) != 2 || seen[0] != "ts-old" || seen[1] != "" {
		t.Fatalf("attempts = %q", seen)
	}
	if !codexTurnStates.foreign("ts-old", "auth-b") {
		t.Fatal("rejected turn state not remembered")
	}
	if codexTurnStates.foreign("ts-new", "auth-b") || !codexTurnStates.foreign("ts-new", "auth-c") {
		t.Fatal("issued turn state not attributed to auth-b")
	}
}

func TestDoCodexHTTPWithRecoveryRetriesBufferLimitOnce(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInsufficientStorage)
		_, _ = io.WriteString(w, "exceeded request buffer limit while retrying upstream")
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	resp, err := doCodexHTTPWithRecovery(context.Background(), srv.Client(), req, "auth-a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if resp.StatusCode != http.StatusInsufficientStorage || !strings.Contains(string(body), "request buffer limit") {
		t.Fatalf("final response = %d %q", resp.StatusCode, body)
	}
}

func TestDoCodexHTTPWithRecoveryLeavesOtherErrorsAlone(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	req.Header.Set("X-Codex-Turn-State", "ts")
	resp, err := doCodexHTTPWithRecovery(context.Background(), srv.Client(), req, "auth-a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if attempts != 1 || !strings.Contains(string(body), "boom") {
		t.Fatalf("attempts=%d body=%q", attempts, body)
	}
}
