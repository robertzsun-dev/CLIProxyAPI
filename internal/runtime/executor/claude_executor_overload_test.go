package executor

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestClassifyClaudeOverloadHasNoCredentialCooldownHint(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After", "30")
	err := classifyClaudeUpstreamError(529, headers, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	var se statusErr
	if !errors.As(err, &se) {
		t.Fatalf("unexpected error type %T", err)
	}
	if se.StatusCode() != 529 {
		t.Fatalf("status = %d, want 529 passed through", se.StatusCode())
	}
	if ra := se.RetryAfter(); ra != nil {
		t.Fatalf("529 produced a cooldown hint %v", *ra)
	}

	// Ordinary errors keep their Retry-After hint.
	err = classifyClaudeUpstreamError(http.StatusServiceUnavailable, headers, []byte(`{}`))
	if !errors.As(err, &se) || se.RetryAfter() == nil || *se.RetryAfter() < 30*time.Second {
		t.Fatalf("503 lost Retry-After: %+v", err)
	}
}
