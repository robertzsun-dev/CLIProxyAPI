package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Codex turn state (x-codex-turn-state) is an opaque sticky-routing token the
// upstream issues per turn. Codex CLI keeps the first value it sees for the rest
// of the turn and echoes it on every request. The token is bound to the auth
// context of the account that issued it, so when the proxy moves a turn to a
// different credential the upstream rejects the request with
// `native turn auth context mismatch`. The registry below remembers which
// credential issued (or rejected) each token so the header is only forwarded to
// the credential that can honour it.

const (
	codexTurnStateHeader      = "X-Codex-Turn-State"
	codexTurnStateRegistryTTL = 12 * time.Hour
	codexTurnStateRegistryMax = 20000
)

type codexTurnStateEntry struct {
	owner    string
	rejected map[string]struct{}
	seen     time.Time
}

type codexTurnStateRegistry struct {
	mu      sync.Mutex
	entries map[string]*codexTurnStateEntry
}

var codexTurnStates = &codexTurnStateRegistry{entries: make(map[string]*codexTurnStateEntry)}

func (r *codexTurnStateRegistry) entryLocked(state string, now time.Time) *codexTurnStateEntry {
	entry := r.entries[state]
	if entry != nil && now.Sub(entry.seen) > codexTurnStateRegistryTTL {
		delete(r.entries, state)
		entry = nil
	}
	if entry == nil {
		if len(r.entries) >= codexTurnStateRegistryMax {
			r.pruneLocked(now)
		}
		entry = &codexTurnStateEntry{}
		r.entries[state] = entry
	}
	entry.seen = now
	return entry
}

func (r *codexTurnStateRegistry) pruneLocked(now time.Time) {
	for key, entry := range r.entries {
		if entry == nil || now.Sub(entry.seen) > codexTurnStateRegistryTTL {
			delete(r.entries, key)
		}
	}
	if len(r.entries) < codexTurnStateRegistryMax {
		return
	}
	// Still full of live entries: drop the oldest half.
	cutoff := now
	for _, entry := range r.entries {
		if entry.seen.Before(cutoff) {
			cutoff = entry.seen
		}
	}
	mid := cutoff.Add(now.Sub(cutoff) / 2)
	for key, entry := range r.entries {
		if !entry.seen.After(mid) {
			delete(r.entries, key)
		}
	}
}

// remember records authID as the issuer of state. The first issuer wins: a
// token is never re-assigned because the upstream only honours it for the
// account that minted it.
func (r *codexTurnStateRegistry) remember(state, authID string) {
	state, authID = strings.TrimSpace(state), strings.TrimSpace(authID)
	if state == "" || authID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entryLocked(state, time.Now())
	if entry.owner == "" {
		if _, rejected := entry.rejected[authID]; !rejected {
			entry.owner = authID
		}
	}
}

// reject records that authID refused state with an auth context mismatch.
func (r *codexTurnStateRegistry) reject(state, authID string) {
	state, authID = strings.TrimSpace(state), strings.TrimSpace(authID)
	if state == "" || authID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entryLocked(state, time.Now())
	if entry.owner == authID {
		entry.owner = ""
	}
	if entry.rejected == nil {
		entry.rejected = make(map[string]struct{})
	}
	entry.rejected[authID] = struct{}{}
}

// foreign reports whether state must not be forwarded to authID: it was issued
// by another credential, or authID already rejected it.
func (r *codexTurnStateRegistry) foreign(state, authID string) bool {
	state, authID = strings.TrimSpace(state), strings.TrimSpace(authID)
	if state == "" || authID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[state]
	if entry == nil || time.Since(entry.seen) > codexTurnStateRegistryTTL {
		return false
	}
	if _, rejected := entry.rejected[authID]; rejected {
		return true
	}
	return entry.owner != "" && entry.owner != authID
}

func (r *codexTurnStateRegistry) reset() {
	r.mu.Lock()
	r.entries = make(map[string]*codexTurnStateEntry)
	r.mu.Unlock()
}

// dropForeignCodexTurnState removes the client's turn-state header when it
// belongs to a different credential than authID.
func dropForeignCodexTurnState(headers http.Header, authID string) {
	if headers == nil {
		return
	}
	state := strings.TrimSpace(headers.Get(codexTurnStateHeader))
	if state == "" {
		return
	}
	if codexTurnStates.foreign(state, authID) {
		headers.Del(codexTurnStateHeader)
		log.Debugf("codex: dropped turn state issued for another credential auth=%s", authID)
	}
}

// rememberCodexTurnStateFromHeaders records the turn state an upstream response
// handed out for authID.
func rememberCodexTurnStateFromHeaders(headers http.Header, authID string) {
	if headers == nil {
		return
	}
	codexTurnStates.remember(headers.Get(codexTurnStateHeader), authID)
}

// observeCodexTurnStateEvent records turn state delivered in-band through a
// codex.response.metadata event.
func observeCodexTurnStateEvent(payload []byte, authID string) {
	if len(payload) == 0 || !bytes.Contains(payload, []byte("codex.response.metadata")) {
		return
	}
	if gjson.GetBytes(payload, "type").String() != "codex.response.metadata" {
		return
	}
	headers := gjson.GetBytes(payload, "headers")
	if !headers.IsObject() {
		return
	}
	headers.ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.String(), codexTurnStateHeader) {
			codexTurnStates.remember(value.String(), authID)
			return false
		}
		return true
	})
}

// isCodexTurnAuthContextMismatch matches the upstream refusal of a turn whose
// server-side state belongs to another account, e.g.
// {"error":{"message":"native turn auth context mismatch: scopes","type":"internal_error"}}.
func isCodexTurnAuthContextMismatch(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), "turn auth context mismatch")
}

// noteCodexTurnStateRejection marks the turn state sent in requestHeaders as
// unusable on authID when body is an auth context mismatch, so the next
// request (or reconnect) for that credential goes out without it.
func noteCodexTurnStateRejection(requestHeaders http.Header, authID string, body []byte) {
	if requestHeaders == nil || !isCodexTurnAuthContextMismatch(body) {
		return
	}
	if state := strings.TrimSpace(requestHeaders.Get(codexTurnStateHeader)); state != "" {
		codexTurnStates.reject(state, authID)
		log.Infof("codex: turn state rejected by credential (auth context mismatch); future requests on auth=%s omit it", authID)
	}
}

// isCodexUpstreamRetryBufferLimit matches the upstream edge refusing to retry a
// request whose body is larger than its retry buffer (HTTP 507). The failure is
// transient and unrelated to the credential.
func isCodexUpstreamRetryBufferLimit(statusCode int, body []byte) bool {
	if statusCode != http.StatusInsufficientStorage {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), "request buffer limit")
}

const codexSameCredentialRetryDelay = 300 * time.Millisecond

// doCodexHTTPWithRecovery sends httpReq and resends it once on the same
// credential for failures a plain resend fixes:
//   - native turn auth context mismatch while a turn-state header was sent:
//     resend without the header (it belongs to another account);
//   - 507 "exceeded request buffer limit while retrying upstream": transient
//     upstream edge failure for large bodies, resend as is.
//
// It also records the turn state the upstream hands out for this credential.
// The returned response always has a readable body.
func doCodexHTTPWithRecovery(ctx context.Context, client *http.Client, httpReq *http.Request, authID string) (*http.Response, error) {
	authID = strings.TrimSpace(authID)
	req := httpReq
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			return resp, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rememberCodexTurnStateFromHeaders(resp.Header, authID)
			// The upstream accepted the client's turn state, so it belongs here.
			codexTurnStates.remember(req.Header.Get(codexTurnStateHeader), authID)
			return resp, nil
		}
		if attempt > 0 || req.GetBody == nil {
			return resp, nil
		}
		data, errRead := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(data))
		if errRead != nil {
			return resp, nil
		}
		sentTurnState := strings.TrimSpace(req.Header.Get(codexTurnStateHeader)) != ""
		mismatch := sentTurnState && isCodexTurnAuthContextMismatch(data)
		bufferLimit := isCodexUpstreamRetryBufferLimit(resp.StatusCode, data)
		if !mismatch && !bufferLimit {
			return resp, nil
		}
		retryReq := req.Clone(req.Context())
		body, errBody := req.GetBody()
		if errBody != nil {
			return resp, nil
		}
		retryReq.Body = body
		if mismatch {
			noteCodexTurnStateRejection(req.Header, authID, data)
			retryReq.Header.Del(codexTurnStateHeader)
			log.Infof("codex executor: resending on auth=%s without foreign turn state after auth context mismatch", authID)
		} else {
			log.Infof("codex executor: upstream retry buffer limit (507) on auth=%s; resending once on the same credential", authID)
			timer := time.NewTimer(codexSameCredentialRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return resp, nil
			case <-timer.C:
			}
		}
		req = retryReq
	}
}
