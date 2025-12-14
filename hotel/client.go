package hotel

import (
	"context"
	"errors"
	"sync"
)

// Client represents a connection to a room. It handles the bidirectional communication
// between the room and the connected client, managing message buffering and delivery.
// Generic type parameters:
// - ClientMetadata: Custom data associated with the client
// - DataType: The type of messages exchanged with the client
type Client[ClientMetadata, DataType any] struct {
	metadata  *ClientMetadata
	bufferCh  chan DataType
	sendCh    chan DataType
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func newClient[ClientMetadata, DataType any](bufferSize int, metadata *ClientMetadata) *Client[ClientMetadata, DataType] {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client[ClientMetadata, DataType]{
		metadata: metadata,
		bufferCh: make(chan DataType, bufferSize),
		sendCh:   make(chan DataType),
		ctx:      ctx,
		cancel:   cancel,
	}
	// Forward event data sent to sendCh (from any goroutine) to a channel that
	// is synchronized to a single goroutine.
	go func() {
		defer close(c.sendCh)
		for {
			select {
			case <-ctx.Done():
				// Drain any remaining messages from bufferCh before closing.
				// Use non-blocking sends so we don't hang if receiver is gone.
				for {
					select {
					case data := <-c.bufferCh:
						select {
						case c.sendCh <- data:
							// Forwarded successfully
						default:
							// Receiver not ready, stop draining
							return
						}
					default:
						// Buffer is empty, done draining
						return
					}
				}
			case data := <-c.bufferCh:
				// Forwarding to sendCh will always block until the user code
				// has read from the Receive() channel. If the buffer channel
				// fills up, then the send method will close the client, which
				// is why we also check the context here.
				select {
				case <-ctx.Done():
					// Try to forward the current message before draining
					select {
					case c.sendCh <- data:
						// Forwarded, now drain the rest
					default:
						// Receiver not ready, give up
						return
					}
					// Drain remaining messages
					for {
						select {
						case data := <-c.bufferCh:
							select {
							case c.sendCh <- data:
							default:
								return
							}
						default:
							return
						}
					}
				case c.sendCh <- data:
					// All good, keep going.
				}
			}
		}
	}()
	return c
}

// Context returns the client's context, which is canceled when the client is closed.
func (c *Client[ClientMetadata, DataType]) Context() context.Context {
	return c.ctx
}

// Metadata returns the client's metadata.
func (c *Client[ClientMetadata, DataType]) Metadata() *ClientMetadata {
	return c.metadata
}

// send queues data to be sent to the client. It returns an error if the client
// is disconnected or if the buffer is full (which also disconnects the client).
func (c *Client[ClientMetadata, DataType]) send(data DataType) error {
	select {
	case <-c.ctx.Done():
		return errors.New("client disconnected")
	case c.bufferCh <- data:
		return nil
	default:
		// Channel is full, disconnect the client
		c.Close()
		return errors.New("send channel full, client disconnected")
	}
}

// Receive returns a channel that provides data sent to this client.
// This channel is closed when the client disconnects.
func (c *Client[ClientMetadata, DataType]) Receive() <-chan DataType {
	// Return the channel that only the internal client goroutine writes to.
	return c.sendCh
}

// Close disconnects the client, which closes the receive channel and cancels the client context.
func (c *Client[ClientMetadata, DataType]) Close() {
	c.closeOnce.Do(func() {
		c.cancel()
	})
}
