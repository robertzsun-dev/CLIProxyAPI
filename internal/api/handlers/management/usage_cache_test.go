package management

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsUsageEndpoint(t *testing.T) {
	cases := []struct {
		method, raw string
		want        bool
	}{
		{http.MethodGet, "https://api.anthropic.com/api/oauth/usage", true},
		{http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", true},
		{http.MethodPost, "https://api.anthropic.com/api/oauth/usage", false},
		{http.MethodGet, "https://api.anthropic.com/v1/messages", false},
		{http.MethodGet, "https://example.com/api/oauth/usage", false},
	}
	for _, tc := range cases {
		u, _ := url.Parse(tc.raw)
		if got := isUsageEndpoint(tc.method, u); got != tc.want {
			t.Errorf("isUsageEndpoint(%s %s) = %v, want %v", tc.method, tc.raw, got, tc.want)
		}
	}
}

func TestUsageCacheSharesFreshReadings(t *testing.T) {
	cache := &usageCache{entries: make(map[string]usageCacheEntry)}
	var calls atomic.Int32
	do := func() (apiCallResponse, error) {
		calls.Add(1)

		return apiCallResponse{StatusCode: http.StatusOK, Body: "fresh"}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := cache.fetch("k", do); err != nil || resp.Body != "fresh" {
				t.Errorf("fetch = %+v, %v", resp, err)
			}
		}()
	}
	wg.Wait()
	if _, err := cache.fetch("k", do); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream called %d times, want 1", n)
	}
}

func TestUsageCacheServesLastGoodReadingOn429(t *testing.T) {
	cache := &usageCache{entries: map[string]usageCacheEntry{
		"k": {resp: apiCallResponse{StatusCode: http.StatusOK, Body: "old"}, fetched: time.Now().Add(-5 * time.Minute)},
	}}
	limited := func() (apiCallResponse, error) {
		return apiCallResponse{StatusCode: http.StatusTooManyRequests, Body: "rate limited"}, nil
	}
	resp, err := cache.fetch("k", limited)
	if err != nil || resp.StatusCode != http.StatusOK || resp.Body != "old" {
		t.Fatalf("expected the stale reading, got %+v, %v", resp, err)
	}
	resp, _ = cache.fetch("other", limited)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("with no earlier reading the 429 must pass through, got %+v", resp)
	}
}

func TestUsageCacheCooldownAndExpiry(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprint(warm), func(t *testing.T) {
			now := time.Now()
			c := &usageCache{entries: make(map[string]usageCacheEntry), now: func() time.Time { return now }}
			if warm {
				c.entries["a"] = usageCacheEntry{resp: apiCallResponse{StatusCode: 200, Body: "old"}, fetched: now.Add(-2 * time.Minute)}
			}
			calls := 0
			fetch := func() (apiCallResponse, error) {
				calls++
				return apiCallResponse{StatusCode: 429, Header: http.Header{"Retry-After": []string{"600"}}}, nil
			}
			for range 3 {
				r, err := c.fetch("a", fetch)
				if err != nil {
					t.Fatal(err)
				}
				if warm && (r.Body != "old" || http.Header(r.Header).Get("X-Quota-Cache") != "stale") {
					t.Fatalf("missing marked stale reading: %+v", r)
				}
				if !warm && r.StatusCode != 429 {
					t.Fatal("must not fabricate quota")
				}
			}
			if calls != 1 {
				t.Fatalf("calls %d", calls)
			}
			now = now.Add(9 * time.Minute)
			c.fetch("a", fetch)
			if calls != 1 {
				t.Fatal("ignored retry-after")
			}
			now = now.Add(2 * time.Minute)
			c.fetch("a", fetch)
			if calls != 2 {
				t.Fatal("did not retry after cooldown")
			}
		})
	}
}
