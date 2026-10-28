package handlers

import (
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// When every credential for a model is cooling down, the 429 tells the client how long to wait.
// That is the true time until the first credential recovers, which can be days (a weekly limit),
// and clients such as Codex sleep inside the request for the whole hint. Credentials usually
// return much sooner than that (quota windows reset, stale cooldowns are cleared), so the hint
// shown to a client is a back-off that starts short and grows with each consecutive 429 that
// client gets for the model, up to a ceiling. It never exceeds the true remaining cooldown.
var cooldownBackoffSteps = []time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
	15 * time.Minute,
}

const (
	// cooldownBackoffQuiet is how long without a cooldown 429 resets a client's back-off.
	cooldownBackoffQuiet = 30 * time.Minute
	// cooldownBackoffJitter spreads clients that fail together so they do not retry in step.
	cooldownBackoffJitter = 0.1
)

type cooldownBackoffEntry struct {
	count int
	last  time.Time
}

type cooldownBackoff struct {
	mu      sync.Mutex
	entries map[string]*cooldownBackoffEntry
	now     func() time.Time
	jitter  func() float64 // in [0,1)
}

func newCooldownBackoff() *cooldownBackoff {
	return &cooldownBackoff{
		entries: make(map[string]*cooldownBackoffEntry),
		now:     time.Now,
		jitter:  rand.Float64,
	}
}

var defaultCooldownBackoff = newCooldownBackoff()

// hint returns the retry hint for the next cooldown 429 to key, given the true remaining cooldown.
func (b *cooldownBackoff) hint(key string, remaining time.Duration) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for k, e := range b.entries {
		if now.Sub(e.last) > cooldownBackoffQuiet {
			delete(b.entries, k)
		}
	}
	e := b.entries[key]
	if e == nil {
		e = &cooldownBackoffEntry{}
		b.entries[key] = e
	}
	step := cooldownBackoffSteps[min(e.count, len(cooldownBackoffSteps)-1)]
	e.count++
	e.last = now
	wait := time.Duration(float64(step) * (1 + cooldownBackoffJitter*(2*b.jitter()-1)))
	wait = min(wait, cooldownBackoffSteps[len(cooldownBackoffSteps)-1])
	return min(wait, remaining)
}

// backoffClientKey identifies the caller of c for back-off purposes.
func backoffClientKey(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get("userApiKey"); ok {
		if key, ok := v.(string); ok && strings.TrimSpace(key) != "" {
			return strings.TrimSpace(key)
		}
	}
	if c.Request != nil {
		return requestClientIP(c.Request)
	}
	return ""
}

// SoftenCooldownRetry replaces the retry hint of a model-cooldown error with the client's
// current back-off step. Other errors are left alone. It is exported for the WebSocket writer.
func SoftenCooldownRetry(clientKey string, err error) {
	model, remaining, ok := coreauth.ModelCooldownRetryIn(err)
	if !ok || remaining <= 0 {
		return
	}
	coreauth.AdvertiseModelCooldown(err, defaultCooldownBackoff.hint(clientKey+"|"+model, remaining))
}

// CapCooldownRetry limits the retry hint of a model-cooldown error to the back-off ceiling,
// without tracking the client. It is for writers that cannot identify their caller.
func CapCooldownRetry(err error) {
	if _, remaining, ok := coreauth.ModelCooldownRetryIn(err); ok && remaining > 0 {
		coreauth.AdvertiseModelCooldown(err, min(remaining, cooldownBackoffSteps[len(cooldownBackoffSteps)-1]))
	}
}
