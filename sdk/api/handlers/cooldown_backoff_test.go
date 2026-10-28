package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func testBackoff(now *time.Time) *cooldownBackoff {
	b := newCooldownBackoff()
	b.now = func() time.Time { return *now }
	b.jitter = func() float64 { return 0.5 } // no jitter
	return b
}

func TestCooldownBackoffEscalatesToCeiling(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := testBackoff(&now)
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		now = now.Add(time.Second)
		if got := b.hint("k|m", 5*24*time.Hour); got != w {
			t.Fatalf("attempt %d: hint = %v, want %v", i+1, got, w)
		}
	}
}

func TestCooldownBackoffNeverExceedsRealCooldown(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := testBackoff(&now)
	if got := b.hint("k|m", 7*time.Second); got != 7*time.Second {
		t.Fatalf("hint = %v, want the real 7s cooldown", got)
	}
}

func TestCooldownBackoffResetsAfterQuietPeriodAndIsPerClient(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := testBackoff(&now)
	for range 4 {
		b.hint("a|m", time.Hour)
	}
	if got := b.hint("b|m", time.Hour); got != 30*time.Second {
		t.Fatalf("another client's first hint = %v, want 30s", got)
	}
	if got := b.hint("a|other", time.Hour); got != 30*time.Second {
		t.Fatalf("another model's first hint = %v, want 30s", got)
	}
	now = now.Add(cooldownBackoffQuiet + time.Minute)
	if got := b.hint("a|m", time.Hour); got != 30*time.Second {
		t.Fatalf("hint after a quiet period = %v, want 30s", got)
	}
	if len(b.entries) != 1 {
		t.Fatalf("expired entries were not dropped: %d left", len(b.entries))
	}
}

func TestCooldownBackoffJitterStaysWithinBounds(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, j := range []float64{0, 0.25, 0.999} {
		b := testBackoff(&now)
		b.jitter = func() float64 { return j }
		got := b.hint("k|m", time.Hour)
		if got < 27*time.Second || got > 33*time.Second {
			t.Fatalf("jitter %v: hint = %v, want within 30s +/-10%%", j, got)
		}
	}
	b := testBackoff(&now)
	b.jitter = func() float64 { return 0.999 }
	for range 8 {
		if got := b.hint("c|m", 24*time.Hour); got > 15*time.Minute {
			t.Fatalf("hint %v is above the 15 minute ceiling", got)
		}
	}
}

func TestWriteErrorResponseShortensModelCooldownHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	defaultCooldownBackoff = newCooldownBackoff()
	handler := NewBaseAPIHandlers(nil, nil)
	post := func() (int, int, int) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Set("userApiKey", "sk-test-client")
		handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
			StatusCode: http.StatusTooManyRequests,
			Error:      coreauth.NewModelCooldownError("gpt-6-astra", "codex", 430859*time.Second),
		})
		header, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
		if err != nil {
			t.Fatalf("Retry-After = %q", recorder.Header().Get("Retry-After"))
		}
		var body struct {
			Error struct {
				ResetSeconds int `json:"reset_seconds"`
			} `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", recorder.Body.String(), err)
		}
		return recorder.Code, header, body.Error.ResetSeconds
	}
	code, first, firstBody := post()
	if code != http.StatusTooManyRequests || first < 27 || first > 33 || firstBody != first {
		t.Fatalf("first 429: code=%d Retry-After=%d reset_seconds=%d, want ~30 and equal", code, first, firstBody)
	}
	_, second, secondBody := post()
	if second < 54 || second > 66 || secondBody != second {
		t.Fatalf("second 429: Retry-After=%d reset_seconds=%d, want ~60 and equal", second, secondBody)
	}
}

func TestWriteErrorResponseLeavesShortCooldownAndOtherErrorsAlone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	defaultCooldownBackoff = newCooldownBackoff()
	handler := NewBaseAPIHandlers(nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("m", "codex", 5*time.Second),
	})
	if got := recorder.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("short cooldown Retry-After = %q, want 5", got)
	}
}
