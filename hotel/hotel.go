package hotel

import (
	"errors"
	"fmt"
	"sync"
)

// maxRoomAttempts is the maximum number of times GetOrCreateRoom will attempt
// to create a room when encountering closed rooms. This prevents infinite loops
// when rooms are closing faster than they can be created (e.g., due to handler bugs).
const maxRoomAttempts = 3

// Hotel manages a collection of virtual rooms that can be created on demand.
// It handles room lifecycle including creation, access, and cleanup when rooms are no longer needed.
// Generic type parameters:
// - RoomMetadata: Custom data associated with each room
// - ClientMetadata: Custom data associated with each client
// - DataType: The type of messages exchanged between clients in rooms
type Hotel[RoomMetadata, ClientMetadata, DataType any] struct {
	mu      sync.RWMutex
	rooms   map[string]*Room[RoomMetadata, ClientMetadata, DataType]
	init    RoomInitFunc[RoomMetadata]
	handler RoomHandlerFunc[RoomMetadata, ClientMetadata, DataType]
}

// New creates a new Hotel instance with the provided room initialization and handler functions.
// The init function will be called when creating a new room to initialize its metadata.
// The handler function will be called to handle room events and logic when a room is created.
func New[RoomMetadata, ClientMetadata, DataType any](init RoomInitFunc[RoomMetadata], handler RoomHandlerFunc[RoomMetadata, ClientMetadata, DataType]) *Hotel[RoomMetadata, ClientMetadata, DataType] {
	return &Hotel[RoomMetadata, ClientMetadata, DataType]{
		rooms:   make(map[string]*Room[RoomMetadata, ClientMetadata, DataType]),
		init:    init,
		handler: handler,
	}
}

// GetOrCreateRoom returns an existing room with the given ID or creates a new one if it doesn't exist.
// If the room initialization fails, the room is cleaned up and an error is returned.
// The room is automatically removed from the hotel when it's closed.
// If the room closes immediately after creation (e.g., due to handler errors), this function
// will attempt up to maxRoomAttempts times before returning an error.
func (h *Hotel[RoomMetadata, ClientMetadata, DataType]) GetOrCreateRoom(id string) (*Room[RoomMetadata, ClientMetadata, DataType], error) {
	if id == "" {
		return nil, errors.New("invalid room id: cannot be empty")
	}

	attempt := 0
	for {
		attempt++
		var room *Room[RoomMetadata, ClientMetadata, DataType]
		var exists bool

		// If a room exists we only need a read lock to retrieve it.
		h.mu.RLock()
		room, exists = h.rooms[id]
		h.mu.RUnlock()

		if !exists {
			// A room might've been created in the short duration between RUnlock()
			// and this code so now we need a write lock where we only create the
			// room if it still doesn't exist.
			h.mu.Lock()
			room, exists = h.rooms[id]
			if !exists {
				room = newRoom(id, h.init, h.handler)
				h.rooms[id] = room
			}
			h.mu.Unlock()
		}

		// Wait for room init to run (or it might've already run in which case this
		// will immediately return nil).
		err := room.initGroup.Wait()

		if !exists {
			// This was the call that created the room, so do additional book
			// keeping once its init has finished and we know if it errored.
			if err != nil {
				h.mu.Lock()
				delete(h.rooms, id)
				h.mu.Unlock()
			} else {
				go func() {
					<-room.ctx.Done()
					h.mu.Lock()
					// Only delete if this room is still in the map (another
					// goroutine may have already replaced it with a new room).
					if h.rooms[room.id] == room {
						delete(h.rooms, room.id)
					}
					h.mu.Unlock()
				}()
			}
		}

		if err != nil {
			return nil, err
		}

		// Check if the room is still usable. If the room was closed between when
		// we retrieved it and now, loop back to create a new one.
		select {
		case <-room.ctx.Done():
			// Room is closed. Help clean it up and retry.
			h.mu.Lock()
			// Only delete if the same room is still in the map (another goroutine
			// may have already replaced it with a new room).
			if h.rooms[id] == room {
				delete(h.rooms, id)
			}
			h.mu.Unlock()

			if attempt >= maxRoomAttempts {
				return nil, fmt.Errorf("room %q closed immediately after creation (attempt %d/%d)", id, attempt, maxRoomAttempts)
			}
			continue
		default:
			return room, nil
		}
	}
}
