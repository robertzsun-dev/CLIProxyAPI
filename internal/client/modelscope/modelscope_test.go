package modelscope

import "testing"

func TestNativeModelFamiliesCatalog(t *testing.T) {
	catalog := []map[string]any{{"id": "claude-fable-5-2"}, {"id": "gpt-6.2-sol"}, {"name": "models/gemini-4-neon"}, {"id": "muse-spark-1.3"}, {"id": "claude-fable-5-dd-foreign"}}
	for _, family := range []string{"claude", "codex", "gemini", "muse"} {
		if got := Catalog(true, family, catalog); len(got) != 1 {
			t.Fatalf("%s: %v", family, got)
		}
	}
	if len(Catalog(false, "claude", catalog)) != len(catalog) || catalog[1]["id"] != "gpt-6.2-sol" {
		t.Fatal("altered shared catalog")
	}
}
