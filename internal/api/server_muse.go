package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const museCodeDefaultOrigin = "https://api.meta.ai"

// museCodeForwardHeaders are the client headers Muse Code's own endpoints
// need; credentials and hop-by-hop headers are never copied.
var museCodeForwardHeaders = []string{"Content-Type", "Accept", "Accept-Encoding", "If-None-Match"}

// museCodeAllowedPath reports whether a Muse Code endpoint may reach Meta.
// Only the model catalog and web search are forwarded; telemetry, feedback,
// diagnostics, remote feature config and anything new stay on this host.
func museCodeAllowedPath(method, path string) bool {
	switch {
	case method == http.MethodGet && path == "/muse-code/models":
		return true
	case strings.HasPrefix(path, "/muse-code/search"):
		return true
	}
	return false
}

// museCodeWithoutContributorModels drops "contributor" models, which share
// session data with Meta for training, from a catalog response.
func museCodeWithoutContributorModels(body []byte) []byte {
	var catalog map[string]json.RawMessage
	if json.Unmarshal(body, &catalog) != nil {
		return body
	}
	var models []map[string]json.RawMessage
	if json.Unmarshal(catalog["data"], &models) != nil {
		return body
	}
	kept := models[:0]
	for _, model := range models {
		var id string
		_ = json.Unmarshal(model["id"], &id)
		if !strings.Contains(strings.ToLower(id), "contributor") {
			kept = append(kept, model)
		}
	}
	data, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	catalog["data"] = data
	out, err := json.Marshal(catalog)
	if err != nil {
		return body
	}
	return out
}

// museCodePassthrough forwards Muse Code's model catalog and web search to
// api.meta.ai with a pooled Meta credential, so a Muse CLI pointed at this
// proxy runs on the proxy's login. Other Muse endpoints are answered locally
// (204) and never leave this host. Chat goes through /v1 and normal routing.
func (s *Server) museCodePassthrough(c *gin.Context) {
	if !museCodeAllowedPath(c.Request.Method, c.Request.URL.Path) {
		c.Status(http.StatusNoContent)
		return
	}
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 16<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}
	ctx := context.WithValue(c.Request.Context(), "gin", c)
	selected, err := s.handlers.AuthManager.SelectAuth(ctx, "meta", "", coreexecutor.Options{Headers: c.Request.Header.Clone()})
	if err != nil || selected == nil {
		status := http.StatusServiceUnavailable
		msg := "no Meta credential available"
		if err != nil {
			status = clienterror.HTTPStatusFromErrorOr(err, status)
			msg = err.Error()
		}
		c.JSON(status, gin.H{"error": msg})
		return
	}

	origin := museCodeDefaultOrigin
	if selected.Attributes != nil {
		if base := strings.TrimSpace(selected.Attributes["base_url"]); base != "" {
			origin = strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
		}
	}
	target := origin + c.Request.URL.Path
	if c.Request.URL.RawQuery != "" {
		target += "?" + c.Request.URL.RawQuery
	}
	headers := make(http.Header)
	catalog := c.Request.URL.Path == "/muse-code/models"
	for _, name := range museCodeForwardHeaders {
		if catalog && (name == "Accept-Encoding" || name == "If-None-Match") {
			continue // the catalog is rewritten, so fetch it plain and in full
		}
		if value := c.GetHeader(name); value != "" {
			headers.Set(name, value)
		}
	}
	req, err := s.handlers.AuthManager.NewHttpRequest(ctx, selected, c.Request.Method, target, body, headers)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	resp, err := s.handlers.AuthManager.HttpRequest(ctx, selected, req)
	if err != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(err, http.StatusBadGateway), gin.H{"error": err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		log.WithField("status", resp.StatusCode).Warnf("muse-code passthrough %s %s failed", c.Request.Method, c.Request.URL.Path)
	}
	if catalog && resp.StatusCode == http.StatusOK {
		upstream, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if errRead != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read model catalog"})
			return
		}
		c.Data(http.StatusOK, "application/json", museCodeWithoutContributorModels(upstream))
		return
	}
	for _, name := range []string{"Content-Type", "Content-Encoding", "ETag", "Cache-Control", "Retry-After"} {
		if value := resp.Header.Get(name); value != "" {
			c.Header(name, value)
		}
	}
	c.Status(resp.StatusCode)
	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, errRead := resp.Body.Read(buf)
		if n > 0 {
			if _, errWrite := c.Writer.Write(buf[:n]); errWrite != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if errRead != nil {
			return
		}
	}
}
