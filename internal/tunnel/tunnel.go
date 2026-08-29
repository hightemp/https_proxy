// Package tunnel manages and relays hijacked CONNECT connections.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

var bufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, 32*1024)
		return &buffer
	},
}

type connectionPair struct {
	client io.Closer
	dest   io.Closer
}

// Registry tracks active tunnels so they can be closed during shutdown.
type Registry struct {
	mu      sync.Mutex
	closing bool
	active  map[*connectionPair]struct{}
	wg      sync.WaitGroup
}

// NewRegistry creates an empty tunnel registry.
func NewRegistry() *Registry {
	return &Registry{active: make(map[*connectionPair]struct{})}
}

// Track registers a tunnel and returns an idempotent release function. It
// rejects new tunnels after CloseAll begins.
func (r *Registry) Track(client, destination io.Closer) (release func(), ok bool) {
	pair := &connectionPair{client: client, dest: destination}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return nil, false
	}
	r.active[pair] = struct{}{}
	r.wg.Add(1)
	return func() { r.untrack(pair) }, true
}

func (r *Registry) untrack(pair *connectionPair) {
	r.mu.Lock()
	if _, ok := r.active[pair]; ok {
		delete(r.active, pair)
		r.wg.Done()
	}
	r.mu.Unlock()
}

// CloseAll prevents new tunnels and closes every currently tracked connection.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	r.closing = true
	active := make([]*connectionPair, 0, len(r.active))
	for pair := range r.active {
		active = append(active, pair)
	}
	r.mu.Unlock()

	for _, pair := range active {
		_ = pair.client.Close()
		_ = pair.dest.Close()
	}
}

// Wait blocks until all tracked tunnels finish or ctx is cancelled.
func (r *Registry) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BufferedConn reads already buffered bytes before reading from its connection.
type BufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

// NewBufferedConn wraps conn with an existing buffered reader.
func NewBufferedConn(conn net.Conn, reader *bufio.Reader) *BufferedConn {
	return &BufferedConn{Conn: conn, reader: reader}
}

// Read implements io.Reader.
func (c *BufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// CloseWrite half-closes the underlying connection when it supports half-close.
func (c *BufferedConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}

// Relay copies data in both directions until both sides finish. A transfer
// error interrupts the opposite direction, a clean EOF uses half-close, and
// inactivity in both directions closes the tunnel after idleTimeout.
func Relay(clientWriter net.Conn, clientSource io.Reader, destination net.Conn, idleTimeout time.Duration) {
	idle := newIdleController(idleTimeout, func() {
		slog.Debug("Tunnel idle timeout reached", "idle_timeout", idleTimeout)
		// Closing both connections is required to interrupt the two copy loops.
		_ = clientWriter.Close()
		_ = destination.Close()
	})
	defer idle.stop()

	results := make(chan error, 2)
	go copyAndHalfClose(destination, activityReader{Reader: clientSource, touch: idle.touch}, results)
	go copyAndHalfClose(clientWriter, activityReader{Reader: destination, touch: idle.touch}, results)

	if firstErr := <-results; firstErr != nil {
		deadline := time.Now()
		_ = clientWriter.SetDeadline(deadline)
		_ = destination.SetDeadline(deadline)
	}
	<-results
}

// RelayStream relays an HTTP/2 CONNECT stream and its destination until both
// directions finish or the tunnel remains idle. The request body represents
// client-to-destination DATA frames; writes to clientWriter become response
// DATA frames.
func RelayStream(
	clientWriter io.Writer,
	clientSource io.ReadCloser,
	destination net.Conn,
	idleTimeout time.Duration,
	flush func() error,
) {
	idle := newIdleController(idleTimeout, func() {
		slog.Debug("Tunnel idle timeout reached", "idle_timeout", idleTimeout)
		_ = clientSource.Close()
		_ = destination.Close()
	})
	defer idle.stop()

	results := make(chan error, 2)
	go copyAndHalfClose(
		destination,
		activityReader{Reader: clientSource, touch: idle.touch},
		results,
	)
	go copyAndHalfClose(
		flushingWriter{Writer: clientWriter, flush: flush},
		activityReader{Reader: destination, touch: idle.touch},
		results,
	)

	if firstErr := <-results; firstErr != nil {
		_ = clientSource.Close()
		_ = destination.Close()
	}
	<-results
}

type flushingWriter struct {
	io.Writer
	flush func() error
}

func (w flushingWriter) Write(buffer []byte) (int, error) {
	written, err := w.Writer.Write(buffer)
	if err != nil || written == 0 || w.flush == nil {
		return written, err
	}
	if flushErr := w.flush(); flushErr != nil {
		return written, flushErr
	}
	return written, nil
}

type activityReader struct {
	io.Reader
	touch func()
}

func (r activityReader) Read(buffer []byte) (int, error) {
	read, err := r.Reader.Read(buffer)
	if read > 0 {
		r.touch()
	}
	return read, err
}

type idleController struct {
	mu        sync.Mutex
	timer     *time.Timer
	timeout   time.Duration
	lastTouch time.Time
	onTimeout func()
	stopped   bool
}

func newIdleController(timeout time.Duration, onTimeout func()) *idleController {
	controller := &idleController{
		timeout:   timeout,
		lastTouch: time.Now(),
		onTimeout: onTimeout,
	}
	controller.timer = time.AfterFunc(timeout, controller.expire)
	return controller
}

func (c *idleController) touch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		c.lastTouch = time.Now()
		c.timer.Reset(c.timeout)
	}
}

func (c *idleController) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		c.stopped = true
		c.timer.Stop()
	}
}

func (c *idleController) expire() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	// A timer callback may already be waiting for the mutex while touch resets
	// it. Re-check the actual last activity to avoid closing an active tunnel.
	if remaining := c.timeout - time.Since(c.lastTouch); remaining > 0 {
		c.timer.Reset(remaining)
		c.mu.Unlock()
		return
	}
	c.stopped = true
	c.mu.Unlock()
	c.onTimeout()
}

func copyAndHalfClose(destination io.Writer, source io.Reader, results chan<- error) {
	written, err := transfer(destination, source)
	if err == nil {
		if conn, ok := destination.(interface{ CloseWrite() error }); ok {
			if closeErr := conn.CloseWrite(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				err = closeErr
			}
		}
	}
	if err != nil {
		slog.Debug("Transfer finished with error", "bytes", written, "error", err)
	} else {
		slog.Debug("Transfer complete", "bytes", written)
	}
	results <- err
}

func transfer(destination io.Writer, source io.Reader) (int64, error) {
	buffer := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(buffer)
	return io.CopyBuffer(destination, source, *buffer)
}
