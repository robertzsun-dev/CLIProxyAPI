package helps

import (
	"bytes"
	"io"
)

// ReplayResponseBody restores a body consumed for recovery inspection, including
// its terminal read error so callers can retain both partial data and diagnostics.
func ReplayResponseBody(data []byte, readErr error) io.ReadCloser {
	return io.NopCloser(&replayedResponseBody{Reader: bytes.NewReader(data), readErr: readErr})
}

type replayedResponseBody struct {
	*bytes.Reader
	readErr error
}

func (r *replayedResponseBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF && r.readErr != nil {
		return n, r.readErr
	}
	return n, err
}
