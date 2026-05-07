// client.go — Go-side WebTransport client mirroring the §3.3.3
// channel multiplex.
//
// The client is the symmetric counterpart of Session: it opens
// control / catch-up bidi streams (writing the channel-id prefix
// first), and accepts events bidi + agent-bytes uni streams (consuming
// the prefix in a per-Client accept loop). Datagrams use the same
// 1-byte prefix.
//
// In v0.1 this is used by:
//
//   - WT package tests — the only way to exercise the channel router
//     end-to-end.
//   - Future internal callers — once a Go-side daemon attaches to a
//     remote WT server (e.g. for cross-host pairing), this is the
//     client.
//
// The Tauri / mobile clients re-implement the same wire convention in
// TypeScript / Kotlin; they do NOT depend on this package.

package wt

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// ClientConfig bundles the dial knobs.
type ClientConfig struct {
	// URL is the wt:// or https:// endpoint (the server mounts /wt).
	URL string

	// TLSClientConfig is the TLS config used for the H3 dial. Required
	// — the caller must supply a RootCAs / InsecureSkipVerify path.
	TLSClientConfig *tls.Config

	// Logger receives operational events. nil = discard.
	Logger *log.Logger
}

// Client is the daemon-internal Go client for a WT server speaking
// the channel-id wire convention. Concurrency-safe: methods may be
// called from multiple goroutines.
type Client struct {
	raw    *webtransport.Session
	logger *log.Logger

	bidiQ map[ChannelID]chan *webtransport.Stream
	uniQ  map[ChannelID]chan *webtransport.ReceiveStream

	mu          sync.Mutex
	closed      bool
	closeQueues sync.Once
	queuesDone  chan struct{}
}

// Dial connects to the WT server at cfg.URL and returns a Client. The
// returned Client owns a goroutine pair (bidi + uni accept loop) that
// runs until Close is called or the server tears the session down.
func Dial(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("wt: ClientConfig.URL required")
	}
	if cfg.TLSClientConfig == nil {
		return nil, errors.New("wt: ClientConfig.TLSClientConfig required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	d := &webtransport.Dialer{
		TLSClientConfig: cfg.TLSClientConfig,
		QUICConfig: &quic.Config{
			MaxIncomingStreams:               64,
			MaxIncomingUniStreams:            64,
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			KeepAlivePeriod:                  5 * time.Second,
		},
	}
	rsp, sess, err := d.Dial(ctx, cfg.URL, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("wt: dial: %w", err)
	}
	if rsp.StatusCode != 200 {
		_ = sess.CloseWithError(0, "bad status")
		return nil, fmt.Errorf("wt: dial: status %d", rsp.StatusCode)
	}

	c := &Client{
		raw:        sess,
		logger:     logger,
		bidiQ:      make(map[ChannelID]chan *webtransport.Stream, 4),
		uniQ:       make(map[ChannelID]chan *webtransport.ReceiveStream, 1),
		queuesDone: make(chan struct{}),
	}
	// Mirror the server's accept queues. Client receives:
	//   events    — server-initiated bidi
	//   catch-up  — only if server initiates a catch-up (reverse path,
	//               rare but legal)
	//   control   — symmetric (server-initiated control RPC, future)
	for _, ch := range []ChannelID{ChannelControl, ChannelEvents, ChannelCatchUp} {
		c.bidiQ[ch] = make(chan *webtransport.Stream, streamCapacity)
	}
	c.uniQ[ChannelAgentBytes] = make(chan *webtransport.ReceiveStream, streamCapacity)

	go c.acceptBidiLoop()
	go c.acceptUniLoop()
	return c, nil
}

// Context returns a context that is closed when the underlying WT
// session ends.
func (c *Client) Context() context.Context { return c.raw.Context() }

// OpenControl opens the client→server control bidi stream, writing
// the channel-id byte before returning it.
func (c *Client) OpenControl(ctx context.Context) (*webtransport.Stream, error) {
	return c.openBidi(ctx, ChannelControl)
}

// AcceptEvents accepts the server-initiated events bidi stream. The
// channel-id byte has already been consumed.
func (c *Client) AcceptEvents(ctx context.Context) (*webtransport.Stream, error) {
	return c.acceptBidi(ctx, ChannelEvents)
}

// OpenCatchUp opens the client→server catch-up bidi stream.
func (c *Client) OpenCatchUp(ctx context.Context) (*webtransport.Stream, error) {
	return c.openBidi(ctx, ChannelCatchUp)
}

// AcceptAgentBytes accepts the server-initiated agent-bytes uni-
// stream. The channel-id byte has already been consumed.
func (c *Client) AcceptAgentBytes(ctx context.Context) (*webtransport.ReceiveStream, error) {
	return c.acceptUni(ctx, ChannelAgentBytes)
}

// SendDatagram sends a datagram tagged with ChannelDatagram.
func (c *Client) SendDatagram(payload []byte) error {
	return sendTaggedDatagram(c.raw, ChannelDatagram, payload)
}

// RecvDatagram blocks until a datagram arrives, validates the prefix,
// and returns the payload.
func (c *Client) RecvDatagram(ctx context.Context) ([]byte, error) {
	return recvTaggedDatagram(c.raw, ctx, ChannelDatagram)
}

// OpenRawTagged is an escape hatch: open a bidi stream with the given
// channel-id prefix even if the channel isn't normally client-
// initiated. Used by the bad-channel-id test to feed garbage into the
// server's accept loop.
func (c *Client) OpenRawTagged(ctx context.Context, tag byte) (*webtransport.Stream, error) {
	str, err := c.raw.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := str.Write([]byte{tag}); err != nil {
		_ = str.Close()
		return nil, err
	}
	return str, nil
}

// Close tears the WT session down. Idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	err := c.raw.CloseWithError(0, "client close")
	c.closeQueues.Do(func() { close(c.queuesDone) })
	return err
}

// openBidi opens a bidi stream and writes the channel-id tag.
func (c *Client) openBidi(ctx context.Context, ch ChannelID) (*webtransport.Stream, error) {
	str, err := c.raw.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := str.Write([]byte{byte(ch)}); err != nil {
		_ = str.Close()
		return nil, fmt.Errorf("wt: write %s tag: %w", ch, err)
	}
	return str, nil
}

func (c *Client) acceptBidi(ctx context.Context, ch ChannelID) (*webtransport.Stream, error) {
	q, ok := c.bidiQ[ch]
	if !ok {
		return nil, fmt.Errorf("wt: no client bidi queue for %s", ch)
	}
	select {
	case str, ok := <-q:
		if !ok {
			return nil, errors.New("wt: client closed")
		}
		return str, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.queuesDone:
		return nil, errors.New("wt: client closed")
	case <-c.raw.Context().Done():
		return nil, errors.New("wt: client closed")
	}
}

func (c *Client) acceptUni(ctx context.Context, ch ChannelID) (*webtransport.ReceiveStream, error) {
	q, ok := c.uniQ[ch]
	if !ok {
		return nil, fmt.Errorf("wt: no client uni queue for %s", ch)
	}
	select {
	case str, ok := <-q:
		if !ok {
			return nil, errors.New("wt: client closed")
		}
		return str, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.queuesDone:
		return nil, errors.New("wt: client closed")
	case <-c.raw.Context().Done():
		return nil, errors.New("wt: client closed")
	}
}

func (c *Client) acceptBidiLoop() {
	defer c.closeQueues.Do(func() { close(c.queuesDone) })
	for {
		str, err := c.raw.AcceptStream(c.raw.Context())
		if err != nil {
			return
		}
		go c.routeIncomingBidi(str)
	}
}

func (c *Client) acceptUniLoop() {
	for {
		str, err := c.raw.AcceptUniStream(c.raw.Context())
		if err != nil {
			return
		}
		go c.routeIncomingUni(str)
	}
}

func (c *Client) routeIncomingBidi(str *webtransport.Stream) {
	tag, err := readChannelID(str)
	if err != nil {
		c.logger.Printf("wt-client: bidi stream tag read: %v", err)
		str.CancelRead(streamErrorCodeBadChannelID)
		str.CancelWrite(streamErrorCodeBadChannelID)
		return
	}
	if !tag.streamCapable() {
		c.logger.Printf("wt-client: bidi stream rejected: %s", tag)
		str.CancelRead(streamErrorCodeBadChannelID)
		str.CancelWrite(streamErrorCodeBadChannelID)
		return
	}
	q, ok := c.bidiQ[tag]
	if !ok {
		c.logger.Printf("wt-client: bidi stream %s arrived but no queue", tag)
		str.CancelRead(streamErrorCodeBadChannelID)
		str.CancelWrite(streamErrorCodeBadChannelID)
		return
	}
	select {
	case q <- str:
	case <-c.raw.Context().Done():
		str.CancelRead(streamErrorCodeBadChannelID)
		str.CancelWrite(streamErrorCodeBadChannelID)
	}
}

func (c *Client) routeIncomingUni(str *webtransport.ReceiveStream) {
	tag, err := readChannelID(str)
	if err != nil {
		c.logger.Printf("wt-client: uni stream tag read: %v", err)
		str.CancelRead(streamErrorCodeBadChannelID)
		return
	}
	if !tag.streamCapable() {
		c.logger.Printf("wt-client: uni stream rejected: %s", tag)
		str.CancelRead(streamErrorCodeBadChannelID)
		return
	}
	q, ok := c.uniQ[tag]
	if !ok {
		c.logger.Printf("wt-client: uni stream %s arrived but no queue", tag)
		str.CancelRead(streamErrorCodeBadChannelID)
		return
	}
	select {
	case q <- str:
	case <-c.raw.Context().Done():
		str.CancelRead(streamErrorCodeBadChannelID)
	}
}
