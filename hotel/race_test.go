package hotel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestMetadataDataRace verifies that Metadata() is safe to call concurrently
// with room initialization. This test should be run with -race.
func TestMetadataDataRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		initStarted := make(chan struct{})
		var initStartedOnce sync.Once

		hotel := New(
			func(ctx context.Context, id string) (*string, error) {
				initStartedOnce.Do(func() {
					close(initStarted)
				})
				// Simulate slow initialization
				time.Sleep(5 * time.Millisecond)
				metadata := "room-metadata"
				return &metadata, nil
			},
			func(ctx context.Context, room *Room[string, string, string]) {
				<-ctx.Done()
			},
		)

		var wg sync.WaitGroup
		roomID := fmt.Sprintf("test-room-%d", i)

		// First goroutine: Creates the room
		wg.Add(1)
		go func() {
			defer wg.Done()
			room, err := hotel.GetOrCreateRoom(roomID)
			if err != nil {
				t.Errorf("Failed to create room: %v", err)
				return
			}
			// Access metadata after init completes
			_ = room.Metadata()
			room.Close()
		}()

		// Wait for init to start
		<-initStarted

		// Second goroutine: Tries to get the same room and access metadata
		// while initialization may still be running
		wg.Add(1)
		go func() {
			defer wg.Done()
			room, err := hotel.GetOrCreateRoom(roomID)
			if err != nil {
				// Room may have been closed, that's OK
				return
			}
			// This should be safe even if called during initialization
			_ = room.Metadata()
		}()

		wg.Wait()
	}
}

// TestGetOrCreateRoomDoesNotReturnClosedRoom verifies that GetOrCreateRoom
// never returns a closed room. Previously, there was a race condition where
// a closed room could be returned before the cleanup goroutine removed it
// from the map.
func TestGetOrCreateRoomDoesNotReturnClosedRoom(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	roomID := "test-closed-room"

	// Run multiple iterations to increase chance of hitting the race
	for i := 0; i < 100; i++ {
		// Create a room
		room1, err := hotel.GetOrCreateRoom(roomID)
		if err != nil {
			t.Fatalf("Failed to create room: %v", err)
		}

		// Close the room
		room1.Close()

		// Immediately try to get the room - this should either:
		// 1. Create a new room (if cleanup already ran)
		// 2. Wait for the old room to be cleaned up and create a new one
		// It should NEVER return the closed room
		room2, err := hotel.GetOrCreateRoom(roomID)
		if err != nil {
			t.Fatalf("Failed to get room after close: %v", err)
		}

		// Verify the room is usable by trying to create a client
		client, err := room2.NewClient(&struct{}{})
		if err != nil {
			t.Errorf("Iteration %d: GetOrCreateRoom returned a closed room: %v", i, err)
			continue
		}

		// Clean up
		room2.RemoveClient(client)
		room2.Close()
	}
}

// TestGetOrCreateRoomConcurrentClose verifies that concurrent calls to
// GetOrCreateRoom handle room closure correctly.
func TestGetOrCreateRoomConcurrentClose(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	roomID := "test-concurrent-close"

	for iteration := 0; iteration < 20; iteration++ {
		// Create a room
		room1, err := hotel.GetOrCreateRoom(roomID)
		if err != nil {
			t.Fatalf("Failed to create room: %v", err)
		}

		// Close it
		room1.Close()

		// Have multiple goroutines try to get/create the room immediately after close
		var wg sync.WaitGroup
		errors := make(chan error, 10)

		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()

				room, err := hotel.GetOrCreateRoom(roomID)
				if err != nil {
					errors <- fmt.Errorf("goroutine %d: failed to get room: %v", id, err)
					return
				}

				// Verify the room is usable
				client, err := room.NewClient(&struct{}{})
				if err != nil {
					errors <- fmt.Errorf("goroutine %d: got closed room: %v", id, err)
					return
				}
				room.RemoveClient(client)
			}(i)
		}

		wg.Wait()
		close(errors)

		for err := range errors {
			t.Error(err)
		}

		// Clean up for next iteration
		room, _ := hotel.GetOrCreateRoom(roomID)
		room.Close()
	}
}

// TestClientMessageDeliveryOnClose verifies that messages queued before
// client close are delivered to the receiver.
func TestClientMessageDeliveryOnClose(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			for {
				select {
				case <-room.Events():
				case <-ctx.Done():
					return
				}
			}
		},
	)

	room, err := hotel.GetOrCreateRoom("test-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Use a small buffer to make the test faster
	client, err := room.NewClientWithBuffer(5, &struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	var wg sync.WaitGroup
	received := make([]string, 0)
	var mu sync.Mutex

	// Start receiver that processes messages slowly
	wg.Add(1)
	go func() {
		defer wg.Done()
		for msg := range client.Receive() {
			mu.Lock()
			received = append(received, msg)
			mu.Unlock()
		}
	}()

	// Send messages
	sentCount := 5
	for i := 0; i < sentCount; i++ {
		msg := fmt.Sprintf("Message %d", i)
		err := room.SendToClient(client, msg)
		if err != nil {
			t.Logf("Failed to send message %d: %v", i, err)
		}
	}

	// Give receiver time to start processing the first message
	time.Sleep(10 * time.Millisecond)

	// Close the client
	client.Close()

	// Wait for receiver to finish
	wg.Wait()

	mu.Lock()
	receivedCount := len(received)
	mu.Unlock()

	// We should receive at least some messages. The exact count depends on timing,
	// but with the drain fix, we should get more than we did before.
	t.Logf("Sent: %d messages, Received: %d messages", sentCount, receivedCount)

	// With the drain fix, we should receive all messages that were successfully
	// queued. Since we send 5 messages and give time for processing to start,
	// we should receive all 5.
	if receivedCount < sentCount {
		t.Errorf("Message loss detected: sent %d messages but only received %d (lost %d)",
			sentCount, receivedCount, sentCount-receivedCount)
	}
}

// TestClientBufferDrain verifies that all buffered messages are delivered
// when a client is closed, even if the receiver is ready.
func TestClientBufferDrain(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			for {
				select {
				case <-room.Events():
				case <-ctx.Done():
					return
				}
			}
		},
	)

	room, err := hotel.GetOrCreateRoom("test-drain")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	client, err := room.NewClientWithBuffer(10, &struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Send messages without starting receiver yet
	sentCount := 5
	for i := 0; i < sentCount; i++ {
		msg := fmt.Sprintf("Message %d", i)
		err := room.SendToClient(client, msg)
		if err != nil {
			t.Fatalf("Failed to send message %d: %v", i, err)
		}
	}

	// Now start receiver and close client almost simultaneously
	var wg sync.WaitGroup
	received := make([]string, 0)
	var mu sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		for msg := range client.Receive() {
			mu.Lock()
			received = append(received, msg)
			mu.Unlock()
		}
	}()

	// Small delay to let receiver start
	time.Sleep(time.Millisecond)

	// Close the client
	client.Close()

	wg.Wait()

	mu.Lock()
	receivedCount := len(received)
	mu.Unlock()

	t.Logf("Sent: %d messages, Received: %d messages", sentCount, receivedCount)

	// With the drain fix, even though we close immediately after starting
	// the receiver, we should receive all messages because they get drained
	if receivedCount != sentCount {
		t.Errorf("Not all messages were delivered: sent %d, received %d", sentCount, receivedCount)
	}
}
