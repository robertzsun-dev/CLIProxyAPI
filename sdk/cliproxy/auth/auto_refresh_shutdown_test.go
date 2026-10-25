package auth

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

type rotatingShutdownExecutor struct {
	countingRefreshExecutor
	started chan context.Context
	release chan struct{}
}

func (e *rotatingShutdownExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	e.started <- ctx
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.release:
	}
	auth.Metadata["access_token"] = "new-access"
	auth.Metadata["refresh_token"] = "new-refresh"
	auth.Metadata["expires_at"] = time.Now().Add(8 * time.Hour).Format(time.RFC3339)
	return auth, nil
}

type shutdownPersistStore struct {
	*memoryAuthTestStore
	started chan context.Context
	release chan struct{}
}

func (s *shutdownPersistStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if auth.Metadata["refresh_token"] == "new-refresh" {
		s.started <- ctx
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-s.release:
		}
	}
	return s.memoryAuthTestStore.Save(ctx, auth)
}

func TestStopAutoRefresh_WaitsForRotatedTokenPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &shutdownPersistStore{
			memoryAuthTestStore: newMemoryAuthTestStore(),
			started:             make(chan context.Context, 1),
			release:             make(chan struct{}),
		}
		manager := NewManager(store, nil, nil)
		executor := &rotatingShutdownExecutor{
			countingRefreshExecutor: countingRefreshExecutor{id: issue6199RefreshProvider},
			started:                 make(chan context.Context, 1),
			release:                 make(chan struct{}),
		}
		manager.RegisterExecutor(executor)
		registerIssue6199ExpiredAuth(t, manager, "rotating", time.Now())
		type refreshContextKey struct{}
		parent, cancel := context.WithCancel(context.WithValue(context.Background(), refreshContextKey{}, "preserved"))
		defer func() {
			for _, release := range []chan struct{}{executor.release, store.release} {
				select {
				case <-release:
				default:
					close(release)
				}
			}
			cancel()
			manager.StopAutoRefresh()
		}()
		manager.StartAutoRefresh(parent, time.Minute)
		refreshCtx := <-executor.started
		cancel()
		stopped := make(chan struct{})
		go func() {
			manager.StopAutoRefresh()
			close(stopped)
		}()
		synctest.Wait()
		if refreshCtx.Err() != nil {
			t.Fatalf("shutdown canceled an in-flight token rotation: %v", refreshCtx.Err())
		}
		if refreshCtx.Value(refreshContextKey{}) != "preserved" {
			t.Fatal("refresh context lost parent values")
		}
		select {
		case <-stopped:
			t.Fatal("shutdown returned before the refresh completed")
		default:
		}
		close(executor.release)
		persistCtx := <-store.started
		synctest.Wait()
		if persistCtx.Err() != nil {
			t.Fatalf("rotated credentials cannot be persisted: %v", persistCtx.Err())
		}
		select {
		case <-stopped:
			t.Fatal("shutdown returned before the rotated tokens were saved")
		default:
		}
		close(store.release)
		<-stopped
		restored := NewManager(store, nil, nil)
		if err := restored.Load(context.Background()); err != nil {
			t.Fatalf("reload saved credentials: %v", err)
		}
		auth, ok := restored.GetByID("rotating")
		if !ok || auth.Metadata["refresh_token"] != "new-refresh" || auth.Metadata["access_token"] != "new-access" {
			t.Fatalf("restart did not load the rotated tokens: %#v", auth)
		}
	})
}

func TestStopAutoRefresh_BoundsUnresponsiveCredentialAcquisition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		executor := &rotatingShutdownExecutor{
			countingRefreshExecutor: countingRefreshExecutor{id: issue6199RefreshProvider},
			started:                 make(chan context.Context, 1),
			release:                 make(chan struct{}),
		}
		manager.RegisterExecutor(executor)
		registerIssue6199ExpiredAuth(t, manager, "unresponsive", time.Now())
		manager.StartAutoRefresh(context.Background(), time.Minute)
		ctx := <-executor.started
		start := time.Now()
		manager.StopAutoRefresh()
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("credential acquisition error = %v, want deadline exceeded", ctx.Err())
		}
		if elapsed := time.Since(start); elapsed != autoRefreshTimeout {
			t.Fatalf("shutdown wait = %v, want %v", elapsed, autoRefreshTimeout)
		}
	})
}

func TestStopAutoRefresh_DoesNotStartQueuedRotations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		executor := &countingRefreshExecutor{id: issue6199RefreshProvider}
		manager.RegisterExecutor(executor)
		registerIssue6199ExpiredAuth(t, manager, "queued", time.Now())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		manager.StartAutoRefresh(ctx, time.Minute)
		manager.StopAutoRefresh()
		if got := executor.refreshCalls.Load(); got != 0 {
			t.Fatalf("shutdown started %d queued rotations", got)
		}
		manager.mu.RLock()
		pending := len(manager.refreshJobs)
		manager.mu.RUnlock()
		if pending != 0 {
			t.Fatalf("shutdown left %d pending refresh jobs", pending)
		}
	})
}
