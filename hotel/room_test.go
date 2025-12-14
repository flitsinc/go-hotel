package hotel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBroadcast verifies that Broadcast sends data to all clients.
func TestBroadcast(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("broadcast-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create multiple clients
	numClients := 5
	clients := make([]*Client[struct{}, string], numClients)
	for i := 0; i < numClients; i++ {
		client, err := room.NewClient(&struct{}{})
		if err != nil {
			t.Fatalf("Failed to create client %d: %v", i, err)
		}
		clients[i] = client
	}

	// Start receivers for all clients
	var wg sync.WaitGroup
	received := make([]int, numClients)
	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c *Client[struct{}, string]) {
			defer wg.Done()
			for range c.Receive() {
				received[idx]++
			}
		}(i, client)
	}

	// Broadcast a message
	room.Broadcast("hello everyone")

	// Give time for messages to be received
	time.Sleep(50 * time.Millisecond)

	// Close all clients to end receivers
	for _, client := range clients {
		client.Close()
	}
	wg.Wait()

	// Verify all clients received the message
	for i, count := range received {
		if count != 1 {
			t.Errorf("Client %d received %d messages, expected 1", i, count)
		}
	}
}

// TestBroadcastRemovesFailedClients verifies that Broadcast removes clients that fail to receive.
func TestBroadcastRemovesFailedClients(t *testing.T) {
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

	room, err := hotel.GetOrCreateRoom("broadcast-fail-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create a client with small buffer
	client, err := room.NewClientWithBuffer(1, &struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Don't read from client - let buffer fill up
	// Broadcast multiple times to fill buffer and trigger removal
	for i := 0; i < 10; i++ {
		room.Broadcast("message")
	}

	// Give time for removal to process
	time.Sleep(50 * time.Millisecond)

	// Client should have been removed
	clients := room.Clients()
	for _, c := range clients {
		if c == client {
			t.Error("Failed client should have been removed from room")
		}
	}
}

// TestBroadcastExcept verifies that BroadcastExcept sends to all clients except one.
func TestBroadcastExcept(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("broadcast-except-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create multiple clients
	numClients := 5
	clients := make([]*Client[struct{}, string], numClients)
	for i := 0; i < numClients; i++ {
		client, err := room.NewClient(&struct{}{})
		if err != nil {
			t.Fatalf("Failed to create client %d: %v", i, err)
		}
		clients[i] = client
	}

	// Start receivers for all clients
	var wg sync.WaitGroup
	received := make([]int, numClients)
	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c *Client[struct{}, string]) {
			defer wg.Done()
			for range c.Receive() {
				received[idx]++
			}
		}(i, client)
	}

	// Broadcast to all except client 2
	excludeIdx := 2
	room.BroadcastExcept(clients[excludeIdx], "hello others")

	// Give time for messages to be received
	time.Sleep(50 * time.Millisecond)

	// Close all clients to end receivers
	for _, client := range clients {
		client.Close()
	}
	wg.Wait()

	// Verify all clients except excluded one received the message
	for i, count := range received {
		if i == excludeIdx {
			if count != 0 {
				t.Errorf("Excluded client %d received %d messages, expected 0", i, count)
			}
		} else {
			if count != 1 {
				t.Errorf("Client %d received %d messages, expected 1", i, count)
			}
		}
	}
}

// TestHandleClientData verifies that HandleClientData emits custom events.
func TestHandleClientData(t *testing.T) {
	eventReceived := make(chan Event[struct{}, string], 10)

	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			for {
				select {
				case event := <-room.Events():
					if event.Type == EventCustom {
						eventReceived <- event
					}
				case <-ctx.Done():
					return
				}
			}
		},
	)

	room, err := hotel.GetOrCreateRoom("handle-data-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Handle client data
	testData := "test message from client"
	err = room.HandleClientData(client, testData)
	if err != nil {
		t.Fatalf("HandleClientData failed: %v", err)
	}

	// Wait for event
	select {
	case event := <-eventReceived:
		if event.Data != testData {
			t.Errorf("Expected data %q, got %q", testData, event.Data)
		}
		if event.Client != client {
			t.Error("Event client doesn't match")
		}
	case <-time.After(time.Second):
		t.Error("Timeout waiting for custom event")
	}
}

// TestHandleClientDataUnknownClient verifies HandleClientData returns error for unknown client.
func TestHandleClientDataUnknownClient(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("handle-data-unknown")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create a client and remove it
	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	room.RemoveClient(client)

	// Try to handle data for removed client
	err = room.HandleClientData(client, "data")
	if err == nil {
		t.Error("Expected error for unknown client")
	}
}

// TestFindClient verifies FindClient returns matching clients.
func TestFindClient(t *testing.T) {
	type ClientMeta struct {
		Name string
		Age  int
	}

	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, ClientMeta, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("find-client-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create clients with different metadata
	alice, _ := room.NewClient(&ClientMeta{Name: "Alice", Age: 30})
	bob, _ := room.NewClient(&ClientMeta{Name: "Bob", Age: 25})
	charlie, _ := room.NewClient(&ClientMeta{Name: "Charlie", Age: 35})

	// Find Alice by name
	found := room.FindClient(func(m *ClientMeta) bool {
		return m.Name == "Alice"
	})
	if found != alice {
		t.Error("Failed to find Alice")
	}

	// Find by age
	found = room.FindClient(func(m *ClientMeta) bool {
		return m.Age > 30
	})
	if found != charlie {
		t.Error("Failed to find client with age > 30")
	}

	// Find non-existent
	found = room.FindClient(func(m *ClientMeta) bool {
		return m.Name == "David"
	})
	if found != nil {
		t.Error("Expected nil for non-existent client")
	}

	_ = bob // Use bob to avoid unused variable warning
}

// TestRoomID verifies the ID() accessor.
func TestRoomID(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	roomID := "test-room-id-123"
	room, err := hotel.GetOrCreateRoom(roomID)
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	if room.ID() != roomID {
		t.Errorf("Expected room ID %q, got %q", roomID, room.ID())
	}
}

// TestRoomInitError verifies that init errors are properly propagated.
func TestRoomInitError(t *testing.T) {
	expectedErr := errors.New("init failed")

	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return nil, expectedErr
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	_, err := hotel.GetOrCreateRoom("init-error-room")
	if err == nil {
		t.Fatal("Expected error from room init")
	}
	if err != expectedErr {
		t.Errorf("Expected error %v, got %v", expectedErr, err)
	}

	// Try again - should get same error (room should have been cleaned up)
	_, err = hotel.GetOrCreateRoom("init-error-room")
	if err == nil {
		t.Fatal("Expected error on second attempt")
	}
}

// TestRemoveClientNotFound verifies RemoveClient returns error for unknown client.
func TestRemoveClientNotFound(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("remove-not-found")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create and remove a client
	client, _ := room.NewClient(&struct{}{})
	room.RemoveClient(client)

	// Try to remove again
	err = room.RemoveClient(client)
	if err == nil {
		t.Error("Expected error when removing non-existent client")
	}
}

// TestSendToClientNotFound verifies SendToClient returns error for unknown client.
func TestSendToClientNotFound(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("send-not-found")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create and remove a client
	client, _ := room.NewClient(&struct{}{})
	room.RemoveClient(client)

	// Try to send to removed client
	err = room.SendToClient(client, "data")
	if err == nil {
		t.Error("Expected error when sending to non-existent client")
	}
}

// TestSendToClientRemovesOnError verifies SendToClient removes client on send error.
func TestSendToClientRemovesOnError(t *testing.T) {
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

	room, err := hotel.GetOrCreateRoom("send-removes-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create client with tiny buffer
	client, err := room.NewClientWithBuffer(1, &struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Don't read from client, fill buffer, then send more
	for i := 0; i < 10; i++ {
		room.SendToClient(client, "msg")
	}

	// Give time for removal
	time.Sleep(50 * time.Millisecond)

	// Verify client was removed
	clients := room.Clients()
	for _, c := range clients {
		if c == client {
			t.Error("Client should have been removed after send error")
		}
	}
}

// TestGetOrCreateRoomEmptyID verifies empty room ID is rejected.
func TestGetOrCreateRoomEmptyID(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	_, err := hotel.GetOrCreateRoom("")
	if err == nil {
		t.Error("Expected error for empty room ID")
	}
}

// TestMultipleBroadcasts verifies multiple sequential broadcasts work correctly.
func TestMultipleBroadcasts(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("multi-broadcast")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	var received atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range client.Receive() {
			received.Add(1)
		}
	}()

	// Send multiple broadcasts
	numMessages := 100
	for i := 0; i < numMessages; i++ {
		room.Broadcast("message")
	}

	// Give time to receive
	time.Sleep(100 * time.Millisecond)
	client.Close()
	wg.Wait()

	if int(received.Load()) != numMessages {
		t.Errorf("Expected %d messages, got %d", numMessages, received.Load())
	}
}
