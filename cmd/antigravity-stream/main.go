// Forward Antigravity's Gemini requests to CLIProxyAPI. agy 1.5.14 rejects
// SSE heartbeat comments, even though the SSE specification permits them.
package main

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

// Read one line at a time without imposing a maximum Gemini chunk size.
type eventStream struct {
	io.Closer
	lines         *bufio.Reader
	pending       []byte
	skipSeparator bool
}

func (s *eventStream) Read(dst []byte) (int, error) {
	for len(s.pending) == 0 {
		line, err := s.lines.ReadBytes('\n')
		if len(line) == 0 {
			return 0, err
		}
		content := bytes.TrimRight(line, "\r\n")
		if bytes.Equal(content, []byte(": keep-alive")) {
			s.skipSeparator = true
			continue
		}
		skip := s.skipSeparator && len(content) == 0
		s.skipSeparator = false
		if !skip {
			s.pending = line
		}
	}
	n := copy(dst, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func main() {
	upstream, err := url.Parse(os.Getenv("CLIPROXY_UPSTREAM"))
	if err != nil {
		log.Fatal(err)
	}
	if upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") {
		log.Fatal("CLIPROXY_UPSTREAM must be an absolute HTTP or HTTPS URL")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(r *http.Response) error {
			media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if media == "text/event-stream" {
				r.Body = &eventStream{Closer: r.Body, lines: bufio.NewReader(r.Body)}
				r.ContentLength = -1
				r.Header.Del("Content-Length")
			}
			return nil
		},
	}
	log.Fatal(http.ListenAndServe("127.0.0.1:18317", proxy))
}
