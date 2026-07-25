package rtc

import (
	"bytes"
	"fmt"
	"net/http"
	"time"
)

// NetHTTP is the real transport. It exists so rtc.LiveKit can be tested against
// a fake without a server, and so the timeout is stated in one place rather
// than inherited from whatever client a caller happens to pass.
type NetHTTP struct {
	Client *http.Client
}

func NewHTTP(timeout time.Duration) *NetHTTP {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &NetHTTP{Client: &http.Client{Timeout: timeout}}
}

func (n *NetHTTP) Do(req *HTTPRequest) (*HTTPResponse, error) {
	r, err := http.NewRequestWithContext(req.Ctx, http.MethodPost, req.Path, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+req.Token)

	res, err := n.Client.Do(r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()

	var buf bytes.Buffer
	// Bounded: a media server answering with an unbounded body is a media
	// server that can exhaust this process's memory from across the network.
	if _, err := buf.ReadFrom(http.MaxBytesReader(nil, res.Body, 1<<20)); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return &HTTPResponse{Status: res.StatusCode, Body: buf.Bytes()}, nil
}

var _ HTTPDoer = (*NetHTTP)(nil)
