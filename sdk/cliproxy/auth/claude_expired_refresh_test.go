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
			if exec.RefreshCalls() != 1 {
				t.Fatal("expired refresh grant was replayed")
			}
		})
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
