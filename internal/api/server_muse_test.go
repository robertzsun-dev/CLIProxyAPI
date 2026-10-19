package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestMuseCodeAllowedPath(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/muse-code/models", true},
		{http.MethodPost, "/muse-code/search/web", true},
		{http.MethodPost, "/muse-code/telemetry/traces", false},
		{http.MethodPost, "/muse-code/telemetry/logs", false},
		{http.MethodPost, "/muse-code/feedback", false},
		{http.MethodGet, "/muse-code/config/v1", false},
		{http.MethodPost, "/muse-code/models", false},
		{http.MethodGet, "/muse-code/browser_open", false},
	}
	for _, tc := range cases {
		if got := museCodeAllowedPath(tc.method, tc.path); got != tc.want {
			t.Errorf("museCodeAllowedPath(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestMuseCodeWithoutContributorModels(t *testing.T) {
	in := []byte(`{"object":"list","data":[{"id":"muse-spark-1.3","x":1},{"id":"muse-spark-1.3-contributor"},{"id":"muse-wonder-full-1.0-Contributor"}]}`)
	var out struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(museCodeWithoutContributorModels(in), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) != 1 || out.Data[0]["id"] != "muse-spark-1.3" || out.Data[0]["x"] != float64(1) {
		t.Fatalf("unexpected catalog: %+v", out)
	}
	if got := museCodeWithoutContributorModels([]byte("not json")); string(got) != "not json" {
		t.Fatalf("non-JSON body should pass through unchanged, got %q", got)
	}
}
