package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClaudeExpiredRefreshGrantStopsRetrying(t *testing.T) {
	for _, accessValid := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired-access", true: "working-access"}[accessValid], func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			exec := &unauthorizedRefreshExecutor{id: "claude", refreshErr: errors.New(`token refresh failed with status 400: {"error":"invalid_grant","error_description":"Refresh token expired"}`)}
			manager.RegisterExecutor(exec)
			expiry := time.Now().Add(-time.Hour)
			if accessValid {
				expiry = time.Now().Add(time.Hour)
			}
			a := &Auth{ID: "claude-expired-refresh", Provider: "claude", Metadata: map[string]any{
				"access_token": "access", "refresh_token": "expired-refresh", "expired": expiry.Format(time.RFC3339),
			}}
			if _, err := manager.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.refreshAuthForRequest(context.Background(), a.ID, ""); err == nil {
				t.Fatal("expired refresh must fail")
			}
			current, _ := manager.GetByID(a.ID)
			if !current.RequiresSignIn {
				t.Fatal("expired refresh grant must request a new sign-in")
			}
			if accessValid {
				if current.Unavailable || current.NextRefreshAfter.IsZero() {
					t.Fatal("working access token must remain usable until expiry")
				}
			} else if !current.Unavailable || !current.NextRefreshAfter.IsZero() {
				t.Fatal("dead credential must be unavailable and unscheduled")
			}
			_, _ = manager.refreshAuthForRequest(context.Background(), a.ID, "")
			for range 3 {
				if _, err := manager.ForceRefreshAuth(context.Background(), a.ID); err == nil {
					t.Fatal("forced refresh must still require a new sign-in")
				}
			}
			if exec.RefreshCalls() != 1 {
				t.Fatal("expired refresh grant was replayed")
			}
		})
	}
}

func TestClaudeFreshLoginAllowsForcedRefreshAfterRejectedGrant(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	exec := &unauthorizedRefreshExecutor{id: "claude", refreshErr: errors.New(`status 400: {"error":"invalid_grant","error_description":"Refresh token expired"}`)}
	manager.RegisterExecutor(exec)
	a := &Auth{ID: t.Name(), Provider: "claude", Metadata: map[string]any{
		"access_token": "expired-access", "refresh_token": "expired-grant", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339),
	}}
	if _, err := manager.Register(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ForceRefreshAuth(ctx, a.ID); err == nil {
		t.Fatal("expected expired grant to fail")
	}
	if _, err := manager.ForceRefreshAuth(ctx, a.ID); err == nil {
		t.Fatal("expected rejected grant to require sign-in")
	}
	if exec.RefreshCalls() != 1 {
		t.Fatal("rejected grant was replayed before sign-in")
	}
	fresh := a.Clone()
	fresh.Metadata["access_token"] = "fresh-access"
	fresh.Metadata["refresh_token"] = "fresh-grant"
	fresh.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	if _, err := manager.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	exec.mu.Lock()
	exec.refreshErr = nil
	exec.mu.Unlock()
	current, err := manager.ForceRefreshAuth(ctx, a.ID)
	if err != nil || current == nil || current.RequiresSignIn || current.Unavailable {
		t.Fatalf("fresh login did not restore refresh: auth=%v error=%v", current != nil, err)
	}
	if exec.RefreshCalls() != 2 {
		t.Fatal("fresh grant was not refreshed exactly once")
	}
}

func TestClaudeRefreshFailureClassification(t *testing.T) {
	a := &Auth{Provider: "claude"}
	for _, message := range []string{"invalid_grant", "temporary refresh failure", "status 429: rate limited", "refresh token expired: status 500"} {
		if isTerminalRefreshError(a, errors.New(message)) {
			t.Errorf("transient or ambiguous failure became terminal: %s", message)
		}
	}
}
