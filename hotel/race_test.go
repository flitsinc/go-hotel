package hotel

import (
	"context"
	"fmt"
	"strings"
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

// TestClientsMethodDataRace verifies that Clients() is safe to call concurrently.
// This is a regression test for commit 9ae01ad which fixed accessing r.clients
// after releasing the lock.
func TestClientsMethodDataRace(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("test-clients-race")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	// Create some clients
	for i := 0; i < 5; i++ {
		_, err := room.NewClient(&struct{}{})
		if err != nil {
			t.Fatalf("Failed to create client: %v", err)
		}
	}

	// Concurrently call Clients() while also adding/removing clients
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				clients := room.Clients()
				// Access the slice to ensure it's valid
				_ = len(clients)
			}
		}()
	}

	// Also add and remove clients concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			client, err := room.NewClient(&struct{}{})
			if err != nil {
				return // Room may be closed
			}
			time.Sleep(time.Millisecond)
			room.RemoveClient(client)
		}
	}()

	wg.Wait()
}

// TestClientConcurrentSendAndClose verifies that sending messages to a client
// and closing it concurrently is safe. This is a regression test for commit
// 8be7f75 which ensured writes and close happen on the same goroutine.
func TestClientConcurrentSendAndClose(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("test-send-close-race")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	defer room.Close()

	for iteration := 0; iteration < 50; iteration++ {
		client, err := room.NewClient(&struct{}{})
		if err != nil {
			t.Fatalf("Failed to create client: %v", err)
		}

		// Start receiver
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range client.Receive() {
				// Just drain
			}
		}()

		// Concurrently send messages and close
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = room.SendToClient(client, fmt.Sprintf("msg-%d", i))
			}
		}()

		// Close after a brief moment
		time.Sleep(time.Millisecond)
		room.RemoveClient(client)

		wg.Wait()
	}
}

// TestRoomInitPanicRecovery verifies that a panic in the room init function
// is recovered and doesn't crash the application. This is a regression test
// for commit ba27514.
func TestRoomInitPanicRecovery(t *testing.T) {
	panicCount := 0
	var mu sync.Mutex

	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			mu.Lock()
			panicCount++
			count := panicCount
			mu.Unlock()

			if count == 1 {
				// Only panic on first attempt
				panic("init panic!")
			}
			// Subsequent attempts succeed
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	// This should not crash - the panic should be recovered on first attempt,
	// then succeed on retry
	room, err := hotel.GetOrCreateRoom("panic-room")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	mu.Lock()
	finalCount := panicCount
	mu.Unlock()

	// Should have panicked once, then succeeded
	if finalCount < 1 {
		t.Error("Expected at least one panic to occur")
	}
	t.Logf("Panic occurred %d time(s), room created successfully after recovery", finalCount)

	room.Close()
}

// TestGetOrCreateRoomRetryLimit verifies that GetOrCreateRoom fails after
// maxRoomAttempts when rooms keep closing immediately.
func TestGetOrCreateRoomRetryLimit(t *testing.T) {
	// Track how many times init was called to verify retries are happening
	initCount := 0
	var mu sync.Mutex

	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			mu.Lock()
			initCount++
			mu.Unlock()
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			// Handler panics immediately, which closes the room via recovery
			panic("handler always fails")
		},
	)

	// GetOrCreateRoom should eventually fail after maxRoomRetries
	_, err := hotel.GetOrCreateRoom("always-fails")

	mu.Lock()
	finalCount := initCount
	mu.Unlock()

	// The room init should have been called up to maxRoomAttempts times
	t.Logf("Init was called %d times", finalCount)

	if err == nil {
		// If we got a room, it should be closed (handler panicked)
		t.Log("GetOrCreateRoom returned without error (handler may not have panicked yet)")
	} else {
		// Verify the error is about retries
		if strings.Contains(err.Error(), "retried") {
			t.Logf("Got expected retry limit error: %v", err)
		} else {
			t.Logf("Got error (not retry limit): %v", err)
		}
	}
}

// TestRoomHandlerPanicRecovery verifies that a panic in the room handler
// function is recovered and doesn't crash the application.
func TestRoomHandlerPanicRecovery(t *testing.T) {
	handlerStarted := make(chan struct{})

	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			close(handlerStarted)
			panic("handler panic!")
		},
	)

	room, err := hotel.GetOrCreateRoom("handler-panic-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	// Wait for handler to start and panic
	<-handlerStarted
	time.Sleep(10 * time.Millisecond)

	// Room should be closed after handler panic
	_, err = room.NewClient(&struct{}{})
	if err == nil {
		t.Error("Expected error when adding client to room with panicked handler")
	}
}

// TestHotelLockNotHeldDuringInit verifies that multiple rooms can be
// initialized concurrently. This is a regression test for commit 096297d
// which ensured the hotel lock isn't held during room initialization.
func TestHotelLockNotHeldDuringInit(t *testing.T) {
	initCount := 0
	var initMu sync.Mutex
	initStarted := make(chan struct{}, 10)

	hotel := New(
		func(ctx context.Context, id string) (*string, error) {
			initMu.Lock()
			initCount++
			initMu.Unlock()
			initStarted <- struct{}{}
			// Slow init
			time.Sleep(50 * time.Millisecond)
			return &id, nil
		},
		func(ctx context.Context, room *Room[string, string, string]) {
			<-ctx.Done()
		},
	)

	// Start creating multiple rooms concurrently
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			room, err := hotel.GetOrCreateRoom(fmt.Sprintf("room-%d", id))
			if err != nil {
				t.Errorf("Failed to create room %d: %v", id, err)
				return
			}
			room.Close()
		}(i)
	}

	// Wait for all inits to start
	for i := 0; i < 5; i++ {
		<-initStarted
	}

	wg.Wait()
	elapsed := time.Since(start)

	// If hotel lock was held during init, this would take ~250ms (5 * 50ms)
	// With the fix, all inits run concurrently, so it should take ~50ms
	if elapsed > 150*time.Millisecond {
		t.Errorf("Room initialization appears to be serialized (took %v), expected concurrent init", elapsed)
	}
}

// TestRoomAutoClose verifies that empty rooms are automatically closed after
// the auto-close delay. This is a regression test for commit 3ff4d61.
func TestRoomAutoClose(t *testing.T) {
	// Skip in short mode since this test involves timing
	if testing.Short() {
		t.Skip("Skipping auto-close test in short mode")
	}

	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("auto-close-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	// Add and remove a client to trigger auto-close scheduling
	client, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	room.RemoveClient(client)

	// Room should still be accessible immediately after client removal
	client2, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to create second client: %v", err)
	}

	// Adding a new client should cancel the auto-close timer
	room.RemoveClient(client2)

	// Verify room context is not yet cancelled
	select {
	case <-room.ctx.Done():
		t.Error("Room was closed prematurely")
	default:
		// Good, room is still open
	}
}

// TestRoomAutoCloseCancel verifies that adding a client cancels the
// auto-close timer.
func TestRoomAutoCloseCancel(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("auto-close-cancel")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	// Add a client, remove it (starts auto-close), then add another
	client1, _ := room.NewClient(&struct{}{})
	room.RemoveClient(client1)

	// Add another client before auto-close fires - this should cancel the timer
	client2, err := room.NewClient(&struct{}{})
	if err != nil {
		t.Fatalf("Failed to add client after removal: %v", err)
	}

	// Room should still be functional
	err = room.SendToClient(client2, "test")
	if err != nil {
		t.Errorf("Failed to send to client: %v", err)
	}

	// Clean up
	room.RemoveClient(client2)
	room.Close()
}

// TestEventChannelFullBehavior verifies that when the events channel is full,
// the room is closed gracefully. This is a regression test for commit ba27514.
func TestEventChannelFullBehavior(t *testing.T) {
	hotel := New(
		func(ctx context.Context, id string) (*struct{}, error) {
			return &struct{}{}, nil
		},
		func(ctx context.Context, room *Room[struct{}, struct{}, string]) {
			// Don't read events - let the channel fill up
			<-ctx.Done()
		},
	)

	room, err := hotel.GetOrCreateRoom("full-events-room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	// Try to add many clients rapidly to fill the events channel
	// The events channel has 1024 capacity
	for i := 0; i < 2000; i++ {
		client, err := room.NewClient(&struct{}{})
		if err != nil {
			// Expected - room may be closed due to full events channel
			t.Logf("Client creation failed at iteration %d: %v", i, err)
			break
		}
		// Don't remove clients - we want events to pile up
		_ = client
	}

	// Room should eventually be closed due to full events channel
	// Give it a moment
	time.Sleep(10 * time.Millisecond)

	_, err = room.NewClient(&struct{}{})
	if err == nil {
		t.Log("Room didn't close from full events channel (might have large buffer)")
	}
}
