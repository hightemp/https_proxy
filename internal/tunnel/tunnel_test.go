package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type closeWriteConn struct {
	net.Conn
	called bool
	err    error
}

func (c *closeWriteConn) CloseWrite() error {
	c.called = true
	return c.err
}

type halfCloseWriter struct {
	buffer    bytes.Buffer
	writeErr  error
	closeErr  error
	closeCall bool
}

func (w *halfCloseWriter) Write(buffer []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.buffer.Write(buffer)
}

func (w *halfCloseWriter) CloseWrite() error {
	w.closeCall = true
	return w.closeErr
}

func TestRegistryClosesTrackedConnectionsAndRejectsNew(t *testing.T) {
	registry := NewRegistry()
	client, clientPeer := net.Pipe()
	destination, destinationPeer := net.Pipe()
	t.Cleanup(func() {
		_ = clientPeer.Close()
		_ = destinationPeer.Close()
	})
	release, tracked := registry.Track(client, destination)
	if !tracked {
		t.Fatal("Track() = false before shutdown")
	}

	registry.CloseAll()
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := registry.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if _, ok := registry.Track(nil, nil); ok {
		t.Fatal("Track() = true after shutdown")
	}

	buffer := make([]byte, 1)
	if _, err := clientPeer.Read(buffer); err == nil {
		t.Fatal("client peer remains open after registry shutdown")
	}
	if _, err := destinationPeer.Read(buffer); err == nil {
		t.Fatal("destination peer remains open after registry shutdown")
	}
}

func TestRegistryWaitHonorsContextCancellation(t *testing.T) {
	registry := NewRegistry()
	client, clientPeer := net.Pipe()
	destination, destinationPeer := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = clientPeer.Close()
		_ = destination.Close()
		_ = destinationPeer.Close()
	})
	release, tracked := registry.Track(client, destination)
	if !tracked {
		t.Fatal("Track() = false before shutdown")
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := registry.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
}

func TestBufferedConnReadsBufferedDataAndForwardsCloseWrite(t *testing.T) {
	base, peer := net.Pipe()
	t.Cleanup(func() {
		_ = base.Close()
		_ = peer.Close()
	})
	closeWriteError := errors.New("close write failed")
	forwarding := &closeWriteConn{Conn: base, err: closeWriteError}
	connection := NewBufferedConn(forwarding, bufio.NewReader(strings.NewReader("buffered data")))

	payload, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if string(payload) != "buffered data" {
		t.Fatalf("Read() = %q, want buffered data", payload)
	}
	if err := connection.CloseWrite(); !errors.Is(err, closeWriteError) {
		t.Fatalf("CloseWrite() error = %v, want %v", err, closeWriteError)
	}
	if !forwarding.called {
		t.Fatal("CloseWrite() was not forwarded")
	}
}

func TestBufferedConnCloseWriteIsOptional(t *testing.T) {
	base, peer := net.Pipe()
	t.Cleanup(func() {
		_ = base.Close()
		_ = peer.Close()
	})
	connection := NewBufferedConn(base, bufio.NewReader(base))
	if err := connection.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error = %v, want nil", err)
	}
}

func TestRelayClosesIdleTunnel(t *testing.T) {
	client, clientPeer := net.Pipe()
	destination, destinationPeer := net.Pipe()
	t.Cleanup(func() {
		_ = clientPeer.Close()
		_ = destinationPeer.Close()
	})

	done := make(chan struct{})
	go func() {
		Relay(client, client, destination, 40*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Relay() did not stop after idle timeout")
	}
}

func TestRelayActivityResetsIdleTimeout(t *testing.T) {
	client, clientPeer := net.Pipe()
	destination, destinationPeer := net.Pipe()
	t.Cleanup(func() {
		_ = clientPeer.Close()
		_ = destinationPeer.Close()
	})

	idleTimeout := 80 * time.Millisecond
	done := make(chan struct{})
	go func() {
		Relay(client, client, destination, idleTimeout)
		close(done)
	}()

	for attempt := 0; attempt < 3; attempt++ {
		if _, err := clientPeer.Write([]byte{'x'}); err != nil {
			t.Fatalf("write activity: %v", err)
		}
		buffer := make([]byte, 1)
		if _, err := destinationPeer.Read(buffer); err != nil {
			t.Fatalf("read relayed activity: %v", err)
		}
		select {
		case <-done:
			t.Fatal("Relay() stopped while tunnel was active")
		default:
		}
		time.Sleep(idleTimeout / 2)
	}

	select {
	case <-done:
	case <-time.After(3 * idleTimeout):
		t.Fatal("Relay() did not stop after activity ceased")
	}
}

func TestRelayStreamCopiesBothDirectionsAndFlushes(t *testing.T) {
	clientSource, clientPeer := net.Pipe()
	destination, destinationPeer := net.Pipe()
	t.Cleanup(func() {
		_ = clientPeer.Close()
		_ = destinationPeer.Close()
	})

	var clientOutput bytes.Buffer
	var flushes atomic.Int32
	done := make(chan struct{})
	go func() {
		RelayStream(
			&clientOutput,
			clientSource,
			destination,
			time.Second,
			func() error {
				flushes.Add(1)
				return nil
			},
		)
		close(done)
	}()

	if _, err := clientPeer.Write([]byte("client-to-destination")); err != nil {
		t.Fatalf("write client stream: %v", err)
	}
	upstreamPayload := make([]byte, len("client-to-destination"))
	if _, err := io.ReadFull(destinationPeer, upstreamPayload); err != nil {
		t.Fatalf("read destination payload: %v", err)
	}
	if string(upstreamPayload) != "client-to-destination" {
		t.Fatalf("destination payload = %q", upstreamPayload)
	}

	if _, err := destinationPeer.Write([]byte("destination-to-client")); err != nil {
		t.Fatalf("write destination stream: %v", err)
	}
	if err := destinationPeer.Close(); err != nil {
		t.Fatalf("close destination peer: %v", err)
	}
	if err := clientPeer.Close(); err != nil {
		t.Fatalf("close client peer: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RelayStream() did not finish after both streams closed")
	}
	if clientOutput.String() != "destination-to-client" {
		t.Fatalf("client payload = %q", clientOutput.String())
	}
	if flushes.Load() == 0 {
		t.Fatal("RelayStream() did not flush response data")
	}
}

func TestRelayStreamClosesIdleStream(t *testing.T) {
	clientSource, clientPeer := net.Pipe()
	destination, destinationPeer := net.Pipe()
	t.Cleanup(func() {
		_ = clientPeer.Close()
		_ = destinationPeer.Close()
	})

	done := make(chan struct{})
	go func() {
		RelayStream(io.Discard, clientSource, destination, 40*time.Millisecond, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RelayStream() did not stop after idle timeout")
	}
}

func TestIdleControllerIgnoresActivityAndExpiryAfterStop(t *testing.T) {
	timedOut := make(chan struct{}, 1)
	controller := newIdleController(time.Hour, func() { timedOut <- struct{}{} })
	controller.stop()
	controller.stop()
	controller.touch()
	controller.expire()

	select {
	case <-timedOut:
		t.Fatal("stopped idle controller invoked timeout callback")
	default:
	}
}

func TestIdleControllerReschedulesPrematureExpiry(t *testing.T) {
	timedOut := make(chan struct{}, 1)
	controller := newIdleController(time.Hour, func() { timedOut <- struct{}{} })
	controller.touch()
	controller.expire()
	controller.stop()

	select {
	case <-timedOut:
		t.Fatal("premature expiry invoked timeout callback")
	default:
	}
}

func TestCopyAndHalfCloseReportsTransferAndCloseErrors(t *testing.T) {
	writeError := errors.New("write failed")
	closeError := errors.New("close write failed")
	tests := []struct {
		name          string
		writer        *halfCloseWriter
		wantError     error
		wantCloseCall bool
	}{
		{name: "success", writer: &halfCloseWriter{}, wantCloseCall: true},
		{name: "write error", writer: &halfCloseWriter{writeErr: writeError}, wantError: writeError},
		{name: "close error", writer: &halfCloseWriter{closeErr: closeError}, wantError: closeError, wantCloseCall: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := make(chan error, 1)
			copyAndHalfClose(test.writer, strings.NewReader("payload"), results)
			err := <-results
			if !errors.Is(err, test.wantError) {
				t.Fatalf("copyAndHalfClose() error = %v, want %v", err, test.wantError)
			}
			if test.writer.closeCall != test.wantCloseCall {
				t.Fatalf("CloseWrite() called = %t, want %t", test.writer.closeCall, test.wantCloseCall)
			}
		})
	}
}
