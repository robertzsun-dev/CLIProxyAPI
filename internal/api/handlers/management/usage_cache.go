package management

import (
	"net/http"
	"net/url"
	"strconv"
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
	mu        sync.Mutex
	entries   map[string]usageCacheEntry
	group     singleflight.Group
	cooldowns map[string]usageCooldown
	now       func() time.Time
}

type usageCooldown struct {
	resp  apiCallResponse
	until time.Time
}

func (c *usageCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
func (c *usageCache) cooling(key string) (apiCallResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.cooldowns[key]
	if !ok || !c.clock().Before(v.until) {
		delete(c.cooldowns, key)
		return apiCallResponse{}, false
	}
	if old, exists := c.entries[key]; exists && c.clock().Sub(old.fetched) <= usageCacheStale {
		resp := old.resp
		resp.Header = http.Header(resp.Header).Clone()
		if resp.Header == nil {
			resp.Header = make(http.Header)
		}
		http.Header(resp.Header).Set("X-Quota-Cache", "stale")
		http.Header(resp.Header).Set("Age", strconv.FormatInt(int64(c.clock().Sub(old.fetched).Seconds()), 10))
		return resp, true
	}
	return v.resp, true
}
func (c *usageCache) cool(key string, resp apiCallResponse) {
	now := c.clock()
	until := now.Add(5 * time.Minute)
	if seconds, err := strconv.Atoi(http.Header(resp.Header).Get("Retry-After")); err == nil && seconds > 0 {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if date, err := http.ParseTime(http.Header(resp.Header).Get("Retry-After")); err == nil && date.After(now) {
		until = date
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cooldowns == nil {
		c.cooldowns = make(map[string]usageCooldown)
	}
	for k, v := range c.cooldowns {
		if !now.Before(v.until) {
			delete(c.cooldowns, k)
		}
	}
	c.cooldowns[key] = usageCooldown{resp: resp, until: until}
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
	if !ok || c.clock().Sub(entry.fetched) > maxAge {
		return apiCallResponse{}, false
	}
	return entry.resp, true
}

func (c *usageCache) put(key string, resp apiCallResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = usageCacheEntry{resp: resp, fetched: c.clock()}
	for k, entry := range c.entries {
		if c.clock().Sub(entry.fetched) > usageCacheStale {
			delete(c.entries, k)
		}
	}
}

// fetch returns a fresh cached reading, or runs do once for all concurrent
// callers. A 429 from upstream is answered with the last good reading if one
// is younger than usageCacheStale.
func (c *usageCache) fetch(key string, do func() (apiCallResponse, error)) (apiCallResponse, error) {
	if resp, ok := c.cooling(key); ok {
		return resp, nil
	}
	if resp, ok := c.get(key, usageCacheFresh); ok {
		return resp, nil
	}
	value, err, _ := c.group.Do(key, func() (any, error) {
		if resp, ok := c.cooling(key); ok {
			return resp, nil
		}
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
			c.cool(key, resp)
			if stale, ok := c.cooling(key); ok {
				return stale, nil
			}
		}
		return resp, nil
	})
	resp, _ := value.(apiCallResponse)
	return resp, err
}
