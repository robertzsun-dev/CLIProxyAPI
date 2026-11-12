package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestV8UsageExpiredAuthOverridesCachedThrottle(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	a := &coreauth.Auth{ID: t.Name(), Provider: "claude", RequiresSignIn: true, Metadata: map[string]any{
		"access_token": "expired-access", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339),
	}}
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	index := a.EnsureIndex()
	var upstreamCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	url := "https://api.anthropic.com/api/oauth/usage"
	registered := (&Handler{authManager: manager}).authByIndex(index)
	apiCallUsageCache.cool(managedUsageCacheKey(index, url, registered), apiCallResponse{StatusCode: 429, Body: "cached throttle"})
	apiCallUsageCache.cool(index+"\x00"+url, apiCallResponse{StatusCode: 429, Body: "legacy throttle"})
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/v8/management/requests/api-call", func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) }, h.APICall)
	router.POST("/legacy", h.APICall)
	for _, path := range []string{"/v8/management/requests/api-call", "/legacy"} {
		body := fmt.Sprintf(`{"auth_index":%q,"method":"GET","url":%q,"proxy_url":%q,"header":{"Authorization":"Bearer $TOKEN$"}}`, index, url, proxy.URL)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("management status: %d: %s", rec.Code, rec.Body.String())
		}
		response := ojson.NewDoc(rec.Body.Bytes())
		if path == "/legacy" {
			if response.Get("status_code").Int() != 429 {
				t.Fatal("deprecated contract changed")
			}
		} else if response.Get("status_code").Int() != 401 || !strings.Contains(response.Get("body").String(), "access token has expired") || !strings.Contains(rec.Body.String(), "sign-in-required") {
			t.Fatalf("expected explicit authentication failure, got %s", rec.Body.String())
		}
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("expired token was sent upstream")
	}
}

func TestV8UsageCacheReplacementLoginAndHeaderCasing(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	a := &coreauth.Auth{ID: t.Name(), Provider: "claude", Metadata: map[string]any{
		"access_token": "old-token", "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	index := a.EnsureIndex()
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer old-token" {
			w.WriteHeader(429)
			_, _ = w.Write([]byte("old token throttled"))
			return
		}
		_, _ = w.Write([]byte(`{"seven_day":{"utilization":10}}`))
	}))
	defer proxy.Close()
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) }, h.APICall)
	fetch := func() int64 {
		t.Helper()
		body := fmt.Sprintf(`{"auth_index":%q,"method":"GET","url":"http://api.anthropic.com/api/oauth/usage","proxy_url":%q,"header":{"authorization":"Bearer $TOKEN$"}}`, index, proxy.URL)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		return ojson.NewDoc(rec.Body.Bytes()).Get("status_code").Int()
	}
	if fetch() != 429 || fetch() != 429 || calls.Load() != 1 {
		t.Fatal("real throttle must be cached, including lowercase authorization header")
	}
	updated := a.Clone()
	updated.Metadata["access_token"] = "new-token"
	if _, err := manager.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if fetch() != 200 || calls.Load() != 2 || fetch() != 200 || calls.Load() != 2 {
		t.Fatal("new login inherited the old throttle or did not cache its healthy reading")
	}
}

func TestUsageCacheIdentityIncludesAccountAndToken(t *testing.T) {
	a := &coreauth.Auth{Metadata: map[string]any{"access_token": "private-token"}}
	b := &coreauth.Auth{Metadata: map[string]any{"access_token": "other-token"}}
	key := managedUsageCacheKey("account-a", "https://api.anthropic.com/api/oauth/usage", a)
	if strings.Contains(key, "private-token") || key == managedUsageCacheKey("account-b", "https://api.anthropic.com/api/oauth/usage", a) || key == managedUsageCacheKey("account-a", "https://api.anthropic.com/api/oauth/usage", b) {
		t.Fatal("cache must isolate credentials without retaining a raw token in its key")
	}
}
