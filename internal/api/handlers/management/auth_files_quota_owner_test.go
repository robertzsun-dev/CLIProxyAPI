package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func patchQuotaOwner(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)
	return rec
}

func TestPatchAuthFileFields_QuotaOwner(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{
		ID: "owner.json", FileName: "owner.json", Provider: "codex",
		Attributes: map[string]string{"path": "/tmp/owner.json"},
		Metadata:   map[string]any{"type": "codex", "email": "a@example.com", "custom_extra": "kept"},
	}
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	if rec := patchQuotaOwner(t, h, `{"name":"owner.json","quota_owner":" Personal "}`); rec.Code != http.StatusOK {
		t.Fatalf("set: status %d body %s", rec.Code, rec.Body.String())
	}
	got, _ := manager.GetByID("owner.json")
	if got.Metadata["quota_owner"] != "personal" || got.Metadata["custom_extra"] != "kept" {
		t.Fatalf("metadata after set = %#v", got.Metadata)
	}
	if entry := h.buildAuthFileEntry(got); entry["quota_owner"] != "personal" {
		t.Fatalf("list entry quota_owner = %#v", entry["quota_owner"])
	}

	for _, bad := range []string{`"boss"`, `3`, `true`} {
		if rec := patchQuotaOwner(t, h, `{"name":"owner.json","quota_owner":`+bad+`}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s: status %d", bad, rec.Code)
		}
	}
	if rec := patchQuotaOwner(t, h, `{"name":"owner.json","quota_owner.x":"coworker"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("nested: status %d", rec.Code)
	}
	got, _ = manager.GetByID("owner.json")
	if got.Metadata["quota_owner"] != "personal" {
		t.Fatalf("invalid patch changed the owner: %#v", got.Metadata["quota_owner"])
	}

	if rec := patchQuotaOwner(t, h, `{"name":"owner.json","quota_owner":"coworker","priority":7}`); rec.Code != http.StatusOK {
		t.Fatalf("coworker: status %d body %s", rec.Code, rec.Body.String())
	}
	got, _ = manager.GetByID("owner.json")
	if got.Metadata["quota_owner"] != "coworker" {
		t.Fatalf("owner = %#v", got.Metadata["quota_owner"])
	}

	if rec := patchQuotaOwner(t, h, `{"name":"owner.json","quota_owner":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: status %d", rec.Code)
	}
	got, _ = manager.GetByID("owner.json")
	if _, ok := got.Metadata["quota_owner"]; ok {
		t.Fatal("quota_owner not cleared")
	}
	if _, ok := h.buildAuthFileEntry(got)["quota_owner"]; ok {
		t.Fatal("cleared quota_owner still listed")
	}
}

// Token refreshes save through the provider token storage; the operator field
// must survive that rewrite.
func TestQuotaOwnerSurvivesTokenStorageSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-a@example.com.json")
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	auth := &coreauth.Auth{
		ID: "claude-a@example.com.json", FileName: "claude-a@example.com.json", Provider: "claude",
		Attributes: map[string]string{"path": path},
		Metadata:   map[string]any{"type": "claude", "email": "a@example.com", "quota_owner": "coworker"},
		Storage:    &claude.ClaudeTokenStorage{AccessToken: "new-token", Email: "a@example.com"},
	}
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if data["quota_owner"] != "coworker" || data["access_token"] != "new-token" {
		t.Fatalf("saved file = %s", raw)
	}
}
