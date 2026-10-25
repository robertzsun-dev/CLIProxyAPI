package cliproxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type startupRefreshExecutor struct {
	coreauth.ProviderExecutor
	calls atomic.Int32
}

func (e *startupRefreshExecutor) Identifier() string { return "startup-refresh-test" }
func (e *startupRefreshExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.calls.Add(1)
	return auth, nil
}

func TestServiceRun_BindFailureDoesNotRefreshCredentials(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	authDir := t.TempDir()
	cfgPath := filepath.Join(authDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &startupRefreshExecutor{}
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "rotating-auth", Provider: executor.Identifier(), Status: coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token": "old-access", "refresh_token": "old-refresh",
			"expires_at":               time.Now().Add(-time.Hour).Format(time.RFC3339),
			"refresh_interval_seconds": 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := NewBuilder().WithConfig(&config.Config{
		Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, AuthDir: authDir,
	}).WithConfigPath(cfgPath).WithCoreAuthManager(manager).Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("service startup error = %v, want occupied address", err)
	}
	if calls := executor.calls.Load(); calls != 0 {
		t.Fatalf("failed startup consumed credentials in %d refresh calls", calls)
	}
}
