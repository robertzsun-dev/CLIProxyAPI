package api

import (
	"context"
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

// museCodePassthrough forwards Muse Code's Meta-only endpoints (model catalog,
// config, search, telemetry, feedback) to api.meta.ai with a pooled Meta
// credential, so a Muse CLI pointed at this proxy runs on the proxy's login.
// Chat traffic still goes through /v1/chat/completions and normal routing.
func (s *Server) museCodePassthrough(c *gin.Context) {
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
	for _, name := range museCodeForwardHeaders {
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
