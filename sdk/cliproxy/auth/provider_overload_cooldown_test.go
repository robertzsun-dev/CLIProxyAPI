package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestMarkResult_ClaudeOverloadDoesNotCoolCredential(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
	}{
		{"status 529", &Error{HTTPStatus: statusProviderOverloaded, Message: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}},
		{"overloaded_error body", &Error{HTTPStatus: http.StatusServiceUnavailable, Message: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{
				ID:         uuid.NewString() + "-claude-overload",
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "test-key"},
			}
			const model = "claude-opus-5-5"
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}
			retryAfter := 30 * time.Second
			manager.MarkResult(context.Background(), Result{
				AuthID:     auth.ID,
				Provider:   "claude",
				Model:      model,
				Success:    false,
				RetryAfter: &retryAfter,
				Error:      tc.err,
			})
			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("auth not found")
			}
			if blocked, reason, next := isAuthBlockedForModel(updated, model, time.Now()); blocked {
				t.Fatalf("overloaded credential blocked: reason=%v next=%v", reason, next)
			}
			if updated.Unavailable || updated.Quota.Exceeded {
				t.Fatalf("overloaded credential marked unavailable: unavailable=%v quota=%+v", updated.Unavailable, updated.Quota)
			}
		})
	}
}

func TestMarkResult_TransientErrorStillCoolsCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:         uuid.NewString() + "-claude-500",
		Provider:   "claude",
		Attributes: map[string]string{"api_key": "test-key"},
	}
	const model = "claude-opus-5-5"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"},
	})
	updated, _ := manager.GetByID(auth.ID)
	if blocked, _, _ := isAuthBlockedForModel(updated, model, time.Now()); !blocked {
		t.Fatal("502 no longer cools the credential; overload exemption is too broad")
	}
}
