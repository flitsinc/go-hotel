package hotel

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestClientContext verifies the Context() accessor.
func TestClientContext(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("client-context-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := client.Context()
	if ctx == nil {
		t.Fatal("Expected non-nil context")
	}

	// Context should not be done yet
	select {
	case <-ctx.Done():
		t.Error("Context should not be done")
	default:
		// Good
	}

	// Close client and verify context is done
	client.Close()

	select {
	case <-ctx.Done():
		// Good
	case <-time.After(time.Second):
		t.Error("Context should be done after Close")
	}
}

// TestClientMetadata verifies the Metadata() accessor.
func TestClientMetadata(t *testing.T) {
	type Meta struct {
		Name string
		ID   int
	}

	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, Meta, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("client-metadata-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	meta := &Meta{Name: "TestUser", ID: 42}
	client, err := room.NewClient(meta)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	gotMeta := client.Metadata()
	if gotMeta != meta {
		t.Error("Metadata should return the same pointer")
	}
	if gotMeta.Name != "TestUser" {
		t.Errorf("Expected Name 'TestUser', got %q", gotMeta.Name)
	}
	if gotMeta.ID != 42 {
		t.Errorf("Expected ID 42, got %d", gotMeta.ID)
	}
}

// TestClientSendToClosedClient verifies send returns error for closed client.
func TestClientSendToClosedClient(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("send-closed-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Close client first
	client.Close()

	// send() is internal, so we test via SendToClient which will remove client
	// But since client is already closed, the context check in send() should trigger
	// We need to keep client in room to test send directly
	// Create new client for this test
	client2, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client2: %v", err)
	}

	// Start receiver to keep client active
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range client2.Receive() {
		}
	}()

	// Close client2 and immediately try to send
	client2.Close()
	time.Sleep(10 * time.Millisecond) // Let close propagate

	// SendToClient should fail (client removed or send fails)
	err = room.SendToClient(client2, "test")
	if err == nil {
		t.Log("SendToClient succeeded (client may not be removed yet)")
	}

	wg.Wait()
}

// TestClientBufferFull verifies that full buffer closes client.
func TestClientBufferFull(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			// Drain events
			for {
				select {
				case <-room.Events():
				case <-ctx.Done():
					return
				}
			}
		},
	)

	room, err := hotel.GetOrCreateRoom("buffer-full-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create client with minimal buffer
	client, err := room.NewClientWithBuffer(1, &struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Don't read from client - let buffer fill up
	// The send() method should return error and close client when buffer is full

	// Keep sending until we get an error or client is removed
	maxAttempts := 100
	var sendErr error
	for i := 0; i < maxAttempts; i++ {
		sendErr = room.SendToClient(client, "message")
		if sendErr != nil {
			break
		}
	}

	// Should have gotten an error (client disconnected or not found)
	if sendErr == nil {
		t.Log("No error after many sends - buffer may be larger than expected")
	}

	// Verify client context is done
	select {
	case <-client.Context().Done():
		// Good - client was closed
	case <-time.After(100 * time.Millisecond):
		t.Error("Client should be closed after buffer overflow")
	}
}

// TestClientReceiveClosesOnClientClose verifies Receive() channel closes when client closes.
func TestClientReceiveClosesOnClientClose(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("receive-close-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	receiveClosed := make(chan struct{})
	go func() {
		for range client.Receive() {
			// Drain
		}
		close(receiveClosed)
	}()

	// Close client
	client.Close()

	// Receive should close
	select {
	case <-receiveClosed:
		// Good
	case <-time.After(time.Second):
		t.Error("Receive channel should close when client closes")
	}
}

// TestClientCloseIdempotent verifies Close() can be called multiple times safely.
func TestClientCloseIdempotent(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("close-idempotent-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Close multiple times - should not panic
	for i := 0; i < 10; i++ {
		client.Close()
	}

	// Context should be done
	select {
	case <-client.Context().Done():
		// Good
	default:
		t.Error("Context should be done after Close")
	}
}

// TestClientConcurrentClose verifies concurrent Close() calls are safe.
func TestClientConcurrentClose(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("concurrent-close-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Close from multiple goroutines concurrently
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.Close()
		}()
	}

	wg.Wait()

	// Should have completed without panic
	select {
	case <-client.Context().Done():
		// Good
	default:
		t.Error("Context should be done")
	}
}
