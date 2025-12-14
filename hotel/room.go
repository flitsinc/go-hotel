package hotel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// RoomInitFunc is a function that initializes a room with the given ID.
// It returns room metadata and any error that occurred during initialization.
type RoomInitFunc[RoomMetadata any] func(ctx context.Context, id string) (metadata *RoomMetadata, err error)

// RoomHandlerFunc is a function that handles room events and operations.
// It is called after a room is successfully initialized.
type RoomHandlerFunc[RoomMetadata, ClientMetadata, DataType any] func(ctx context.Context, room *Room[RoomMetadata, ClientMetadata, DataType])

// Room represents a virtual space where clients can connect and interact.
// It manages client connections, message delivery, and room lifecycle.
// Generic type parameters:
// - RoomMetadata: Custom data associated with the room
// - ClientMetadata: Custom data associated with each client in the room
// - DataType: The type of messages exchanged between clients in the room
type Room[RoomMetadata, ClientMetadata, DataType any] struct {
	initGroup errgroup.Group

	id           string
	metadata     *RoomMetadata
	clients      map[*Client[ClientMetadata, DataType]]struct{}
	mu           sync.RWMutex
	ctx          context.Context
	cancel       context.CancelFunc
	eventsCh     chan Event[ClientMetadata, DataType]
	closeTimer   *time.Timer
	closeTimerMu sync.Mutex
}

// TODO: This should be configurable on either a per-room or global basis.
const DefaultAutoCloseDelay = 2 * time.Minute

func newRoom[RoomMetadata, ClientMetadata, DataType any](id string, init RoomInitFunc[RoomMetadata], handler RoomHandlerFunc[RoomMetadata, ClientMetadata, DataType]) *Room[RoomMetadata, ClientMetadata, DataType] {
	ctx, cancel := context.WithCancel(context.Background())
	eventsCh := make(chan Event[ClientMetadata, DataType], 1024)
	room := &Room[RoomMetadata, ClientMetadata, DataType]{
		id:       id,
		clients:  make(map[*Client[ClientMetadata, DataType]]struct{}),
		ctx:      ctx,
		cancel:   cancel,
		eventsCh: eventsCh,
	}
	room.initGroup.Go(func() error {
		defer func() {
			if err := recover(); err != nil {
				const size = 64 << 10
				buf := make([]byte, size)
				buf = buf[:runtime.Stack(buf, false)]
				log.Printf("Room %s init panicked: %v\n%s", room.id, err, buf)
				room.Close()
			}
		}()

		metadata, err := init(ctx, id)
		if err != nil {
			return err
		}
		// TODO: We should return as soon as the context is cancelled, rather
		// than waiting on the init function to return.
		if err := ctx.Err(); err != nil {
			return err
		}
		room.mu.Lock()
		room.metadata = metadata
		room.mu.Unlock()

		go func() {
			defer func() {
				if err := recover(); err != nil {
					const size = 64 << 10
					buf := make([]byte, size)
					buf = buf[:runtime.Stack(buf, false)]
					log.Printf("Room %s handler panicked: %v\n%s", room.id, err, buf)
				}
				room.Close()
			}()
			handler(ctx, room)
		}()
		return nil
	})
	return room
}

// ID returns the room's unique identifier.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) ID() string {
	return r.id
}

// Events returns a channel that provides events occurring in the room,
// such as clients joining, leaving, or sending custom data.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) Events() <-chan Event[ClientMetadata, DataType] {
	return r.eventsCh
}

// Metadata returns the room's metadata.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) Metadata() *RoomMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.metadata
}

// NewClient creates and adds a new client to the room with the given metadata.
// It emits a join event and cancels any scheduled room closure. Returns an
// error if the room is closed.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) NewClient(metadata *ClientMetadata) (*Client[ClientMetadata, DataType], error) {
	return r.NewClientWithBuffer(256, metadata)
}

// NewClientWithBuffer creates and adds a new client to the room with the given
// buffer size and metadata. It emits a join event and cancels any scheduled
// room closure. Returns an error if the room is closed.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) NewClientWithBuffer(bufferSize int, metadata *ClientMetadata) (*Client[ClientMetadata, DataType], error) {
	r.mu.Lock()
	select {
	case <-r.ctx.Done():
		r.mu.Unlock()
		return nil, errors.New("cannot add client: room is closed")
	default:
		// Cancel any pending close timer
		r.cancelCloseTimer()

		client := newClient[ClientMetadata, DataType](bufferSize, metadata)
		newClients := make(map[*Client[ClientMetadata, DataType]]struct{}, len(r.clients)+1)
		for c := range r.clients {
			newClients[c] = struct{}{}
		}
		newClients[client] = struct{}{}
		r.clients = newClients
		r.mu.Unlock()
		r.Emit(Event[ClientMetadata, DataType]{
			Type:   EventJoin,
			Client: client,
		})
		return client, nil
	}
}

// RemoveClient removes a client from the room, emits a leave event, and closes the client.
// If this was the last client, it schedules room closure after a delay.
// Returns an error if the client is not found in the room.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) RemoveClient(client *Client[ClientMetadata, DataType]) error {
	r.mu.Lock()
	if _, exists := r.clients[client]; !exists {
		r.mu.Unlock()
		return fmt.Errorf("client not found")
	}
	newClients := make(map[*Client[ClientMetadata, DataType]]struct{}, len(r.clients)-1)
	for c := range r.clients {
		if c != client {
			newClients[c] = struct{}{}
		}
	}
	r.clients = newClients
	isEmpty := len(newClients) == 0
	r.mu.Unlock()

	r.Emit(Event[ClientMetadata, DataType]{
		Type:   EventLeave,
		Client: client,
	})
	client.Close()

	// Schedule room closure if empty
	if isEmpty {
		r.scheduleClose()
	}
	return nil
}

// Emit sends an event to the room's event channel.
// If the channel is full, it logs a warning and closes the room.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) Emit(event Event[ClientMetadata, DataType]) {
	select {
	case r.eventsCh <- event:
	default:
		log.Printf("Warning: Room %s events channel is full. Cannot send %s. Closing room.", r.id, event.Type)
		r.Close()
	}
}

// HandleClientData processes data received from a client and emits it as a custom event.
// Returns an error if the client is not found in the room.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) HandleClientData(client *Client[ClientMetadata, DataType], data DataType) error {
	r.mu.RLock()
	_, exists := r.clients[client]
	r.mu.RUnlock()
	if !exists {
		return fmt.Errorf("client not found")
	}
	r.Emit(Event[ClientMetadata, DataType]{
		Type:   EventCustom,
		Client: client,
		Data:   data,
	})
	return nil
}

// SendToClient sends data to a specific client in the room.
// If the client is disconnected or there's an error sending, it removes the client from the room.
// Returns an error if the client is not found or if sending fails.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) SendToClient(client *Client[ClientMetadata, DataType], data DataType) error {
	r.mu.RLock()
	_, exists := r.clients[client]
	r.mu.RUnlock()
	if !exists {
		return fmt.Errorf("client not found")
	}
	if err := client.send(data); err != nil {
		r.RemoveClient(client)
		return fmt.Errorf("failed to send data: %w", err)
	}
	return nil
}

// Broadcast sends data to all clients in the room.
// If any client is disconnected or there's an error sending, it removes that client.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) Broadcast(data DataType) {
	r.mu.RLock()
	clients := r.clients
	r.mu.RUnlock()
	for client := range clients {
		if err := client.send(data); err != nil {
			r.RemoveClient(client)
			log.Printf("Failed to send data to client %p: %v", client, err)
		}
	}
}

// BroadcastExcept sends data to all clients in the room except the specified one.
// If any client is disconnected or there's an error sending, it removes that client.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) BroadcastExcept(except *Client[ClientMetadata, DataType], data DataType) {
	r.mu.RLock()
	clients := r.clients
	r.mu.RUnlock()
	for client := range clients {
		if client != except {
			if err := client.send(data); err != nil {
				r.RemoveClient(client)
				log.Printf("Failed to send data to client %p: %v", client, err)
			}
		}
	}
}

// Close shuts down the room, cancels any scheduled close timer, and closes all clients.
// After this method is called, the room cannot be used anymore.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) Close() {
	r.cancelCloseTimer()
	r.cancel()
	r.mu.Lock()
	for client := range r.clients {
		client.Close()
	}
	r.clients = nil
	r.mu.Unlock()
	// TODO: Figure out if/when we should close the events channel. Close() is
	// public and so are methods writing to the channel, so it's very difficult
	// to prove that writes and close happen on the same goroutine.
	// close(r.eventsCh)
}

// FindClient returns the first client whose metadata matches the given predicate.
// Returns nil if no matching client is found.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) FindClient(predicate func(*ClientMetadata) bool) *Client[ClientMetadata, DataType] {
	r.mu.RLock()
	clients := r.clients
	r.mu.RUnlock()
	for client := range clients {
		if predicate(client.Metadata()) {
			return client
		}
	}
	return nil
}

// Clients returns a slice containing all the clients currently in the room.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) Clients() []*Client[ClientMetadata, DataType] {
	r.mu.RLock()
	clients := r.clients
	r.mu.RUnlock()
	clientsSlice := make([]*Client[ClientMetadata, DataType], 0, len(clients))
	for client := range clients {
		clientsSlice = append(clientsSlice, client)
	}
	return clientsSlice
}

// scheduleClose sets a timer to close the room after a delay if it remains empty.
// This is used for automatic cleanup of unused rooms.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) scheduleClose() {
	r.closeTimerMu.Lock()
	defer r.closeTimerMu.Unlock()

	if r.closeTimer != nil {
		r.closeTimer.Stop()
	}
	r.closeTimer = time.AfterFunc(DefaultAutoCloseDelay, func() {
		r.mu.RLock()
		isEmpty := len(r.clients) == 0
		r.mu.RUnlock()

		if isEmpty {
			r.Close()
		}
	})
}

// cancelCloseTimer cancels any pending room close timer.
// This is called when clients join the room or the room is manually closed.
func (r *Room[RoomMetadata, ClientMetadata, DataType]) cancelCloseTimer() {
	r.closeTimerMu.Lock()
	defer r.closeTimerMu.Unlock()

	if r.closeTimer != nil {
		r.closeTimer.Stop()
		r.closeTimer = nil
	}
}
