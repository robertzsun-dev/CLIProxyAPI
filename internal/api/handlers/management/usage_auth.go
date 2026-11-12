package management

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func usesManagedUsageToken(headers map[string]string) bool {
	for name, value := range headers {
		if strings.EqualFold(name, "Authorization") {
			return strings.EqualFold(strings.TrimSpace(value), "Bearer $TOKEN$")
		}
	}
	return false
}

// A replacement login must not inherit an earlier token's cached 429 or quota.
// Only a fingerprint enters the key; credentials are never stored in cache keys.
func managedUsageCacheKey(authIndex, rawURL string, auth *coreauth.Auth) string {
	fingerprint := sha256.Sum256([]byte(tokenValueForAuth(auth)))
	return authIndex + "\x00" + rawURL + "\x00" + hex.EncodeToString(fingerprint[:])
}
