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
