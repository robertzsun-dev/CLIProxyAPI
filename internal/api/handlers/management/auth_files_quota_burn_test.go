package management

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPatchAuthFileFields_QuotaBurn(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{
		ID: "burn.json", FileName: "burn.json", Provider: "claude",
		Attributes: map[string]string{"path": "/tmp/burn.json"},
		Metadata:   map[string]any{"type": "claude", "email": "a@example.com", "quota_owner": "coworker"},
	}
	if _, err := manager.Register(context.Background(), record); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := &Handler{authManager: manager}
	burnOf := func() any {
		got, _ := manager.GetByID("burn.json")
		return h.buildAuthFileEntry(got, authFileQuotaContext{})["quota_burn"]
	}

	if rec := patchQuotaOwner(t, h, `{"name":"burn.json","quota_burn":true}`); rec.Code != http.StatusOK {
		t.Fatalf("patch true: %d %s", rec.Code, rec.Body.String())
	}
	if burnOf() != true {
		t.Fatalf("listed quota_burn = %#v, want true", burnOf())
	}
	if rec := patchQuotaOwner(t, h, `{"name":"burn.json","quota_burn":1793000000}`); rec.Code != http.StatusOK {
		t.Fatalf("patch time: %d %s", rec.Code, rec.Body.String())
	}
	if burnOf() != int64(1793000000) {
		t.Fatalf("listed quota_burn = %#v, want 1793000000", burnOf())
	}
	for _, bad := range []string{`"yes"`, `-5`, `1.5`, `{"a":1}`} {
		if rec := patchQuotaOwner(t, h, `{"name":"burn.json","quota_burn":`+bad+`}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("quota_burn %s: status %d, want 400", bad, rec.Code)
		}
	}
	if burnOf() != int64(1793000000) {
		t.Fatalf("invalid patch changed quota_burn: %#v", burnOf())
	}
	for _, off := range []string{`false`, `null`} {
		patchQuotaOwner(t, h, `{"name":"burn.json","quota_burn":true}`)
		if rec := patchQuotaOwner(t, h, `{"name":"burn.json","quota_burn":`+off+`}`); rec.Code != http.StatusOK {
			t.Fatalf("patch %s: %d", off, rec.Code)
		}
		if got := burnOf(); got != nil {
			t.Fatalf("quota_burn after %s = %#v, want unset", off, got)
		}
	}
}

func TestNormalizeQuotaBurnFromFile(t *testing.T) {
	// Values loaded from an auth file decode as float64.
	var data map[string]any
	_ = json.Unmarshal([]byte(`{"a":1793000000,"b":true,"c":0}`), &data)
	if v, ok := normalizeQuotaBurn(data["a"]); !ok || v != int64(1793000000) {
		t.Fatalf("a = %#v, %v", v, ok)
	}
	if v, ok := normalizeQuotaBurn(data["b"]); !ok || v != true {
		t.Fatalf("b = %#v, %v", v, ok)
	}
	if _, ok := normalizeQuotaBurn(data["c"]); ok {
		t.Fatal("0 must be rejected")
	}
}
