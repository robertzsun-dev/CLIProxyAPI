package executor

import (
	"errors"
	"net/http"
)

// UpstreamWebsocketReplayRequiredError indicates that an incremental request
// cannot safely continue because its upstream websocket is no longer reusable.
type UpstreamWebsocketReplayRequiredError struct{}

func (*UpstreamWebsocketReplayRequiredError) Error() string {
	return `{"error":{"message":"upstream transport requires full HTTP replay","type":"server_error","code":"upstream_http_replay_required","status":426}}`
}

func (*UpstreamWebsocketReplayRequiredError) StatusCode() int { return http.StatusUpgradeRequired }

func (*UpstreamWebsocketReplayRequiredError) IsRequestScoped() bool { return true }

// NewUpstreamWebsocketReplayRequiredError creates a request-scoped replay signal.
func NewUpstreamWebsocketReplayRequiredError() error {
	return &UpstreamWebsocketReplayRequiredError{}
}

// IsUpstreamWebsocketReplayRequired reports whether err is the internal replay signal.
func IsUpstreamWebsocketReplayRequired(err error) bool {
	var replayErr *UpstreamWebsocketReplayRequiredError
	return errors.As(err, &replayErr)
}

// WebsocketCredentialFailoverError reports that the upstream refused a queued
// request on a bound duplex websocket because its credential is out of quota,
// before any event of that request reached the client. Cause keeps the
// upstream classification (status, retry-after, credential scope) so the
// conductor cools the credential as usual. Frames holds the client frames that
// were not answered, oldest first, so the downstream handler can replay them
// on another credential. ErrorPayload is the upstream error event to hand to
// the client if no other credential can take the replay.
type WebsocketCredentialFailoverError struct {
	Cause        error
	Frames       [][]byte
	ErrorPayload []byte
}

func (e *WebsocketCredentialFailoverError) Error() string {
	if e == nil || e.Cause == nil {
		return "websocket credential failover"
	}
	return e.Cause.Error()
}

func (e *WebsocketCredentialFailoverError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// AsWebsocketCredentialFailover extracts a credential failover signal from err.
func AsWebsocketCredentialFailover(err error) (*WebsocketCredentialFailoverError, bool) {
	var failover *WebsocketCredentialFailoverError
	if errors.As(err, &failover) && failover != nil {
		return failover, true
	}
	return nil, false
}
