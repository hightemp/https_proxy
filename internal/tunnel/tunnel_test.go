package tunnel

import (
	"context"
	"net"
	"testing"
	"time"
)

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
