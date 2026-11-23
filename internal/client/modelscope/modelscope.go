// Package modelscope limits native client catalogs and requests to their model family.
package modelscope

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

func Allows(family, model string) bool {
	id := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	id = strings.TrimPrefix(id, "models/")
	switch family {
	case "claude":
		return strings.HasPrefix(id, "claude-") && !strings.HasPrefix(id, "claude-fable-5-dd-")
	case "gemini":
		return strings.HasPrefix(id, "gemini-")
	case "codex":
		return (strings.HasPrefix(id, "gpt-") && !strings.HasPrefix(id, "gpt-oss-")) ||
			strings.HasPrefix(id, "codex-") || (len(id) >= 2 && id[0] == 'o' &&
			id[1] >= '1' && id[1] <= '9' && (len(id) == 2 || id[2] == '-'))
	case "muse":
		return strings.HasPrefix(id, "muse-")
	default:
		return true
	}
}

// Catalog leaves the shared registry slice intact for clients using other protocols.
func Catalog(enabled bool, family string, models []map[string]any) []map[string]any {
	if !enabled {
		return models
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if id == "" {
			id, _ = model["name"].(string)
		}
		if Allows(family, id) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}
