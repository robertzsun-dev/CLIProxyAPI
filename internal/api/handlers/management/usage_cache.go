package management

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Several dashboards (T3 servers, the quota guard, the panel) poll account usage
// through api-call. Anthropic rate-limits its usage endpoint per account, so
// identical polls share one upstream fetch and fall back to the last good reading.
const (
	usageCacheFresh = 90 * time.Second
	usageCacheStale = 30 * time.Minute
)

type usageCacheEntry struct {
	resp    apiCallResponse
	fetched time.Time
}

type usageCache struct {
	mu      sync.Mutex
	entries map[string]usageCacheEntry
	group   singleflight.Group
}

var apiCallUsageCache = &usageCache{entries: make(map[string]usageCacheEntry)}

// isUsageEndpoint reports whether a GET to u reads an account's usage limits.
func isUsageEndpoint(method string, u *url.URL) bool {
	if method != http.MethodGet || u == nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "api.anthropic.com" && u.Path == "/api/oauth/usage":
		return true
	case host == "chatgpt.com" && strings.HasSuffix(u.Path, "/wham/usage"):
		return true
	}
	return false
}

func (c *usageCache) get(key string, maxAge time.Duration) (apiCallResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Since(entry.fetched) > maxAge {
		return apiCallResponse{}, false
	}
	return entry.resp, true
}

func (c *usageCache) put(key string, resp apiCallResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = usageCacheEntry{resp: resp, fetched: time.Now()}
	for k, entry := range c.entries {
		if time.Since(entry.fetched) > usageCacheStale {
			delete(c.entries, k)
		}
	}
}

// fetch returns a fresh cached reading, or runs do once for all concurrent
// callers. A 429 from upstream is answered with the last good reading if one
// is younger than usageCacheStale.
func (c *usageCache) fetch(key string, do func() (apiCallResponse, error)) (apiCallResponse, error) {
	if resp, ok := c.get(key, usageCacheFresh); ok {
		return resp, nil
	}
	value, err, _ := c.group.Do(key, func() (any, error) {
		if resp, ok := c.get(key, usageCacheFresh); ok {
			return resp, nil
		}
		resp, err := do()
		if err != nil {
			return resp, err
		}
		if resp.StatusCode == http.StatusOK {
			c.put(key, resp)
		} else if resp.StatusCode == http.StatusTooManyRequests {
			if stale, ok := c.get(key, usageCacheStale); ok {
				return stale, nil
			}
		}
		return resp, nil
	})
	resp, _ := value.(apiCallResponse)
	return resp, err
}
