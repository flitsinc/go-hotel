package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/flitsinc/go-hotel/hotel"
	"github.com/gorilla/websocket"
)

// RoomMetadata contains metadata about a chat room
type RoomMetadata struct {
	Name string
}

// UserMetadata contains metadata about a connected user
type UserMetadata struct {
	Name string
}

// Message types that implement the hotel.Message interface
type JoinMessage struct {
	Name string `json:"name"`
}

func (m JoinMessage) Type() string {
	return "join"
}

type LeaveMessage struct {
	Name string `json:"name"`
}

func (m LeaveMessage) Type() string {
	return "leave"
}

type ChatMessage struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (m ChatMessage) Type() string {
	return "chat"
}

// Global room manager instance
var roomManager = hotel.New(roomInit, roomHandler)

// WebSocket connection upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Implement proper origin checking in production
	},
}

// Message registry for type handling
var messageRegistry = hotel.MessageRegistry[hotel.Message]{}

// Initialize message types
func init() {
	messageRegistry.Register(
		&JoinMessage{},
		&LeaveMessage{},
		&ChatMessage{},
	)
}

func main() {
	http.HandleFunc("/ws/", serveWs)
	log.Println("Server started on :8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe:", err)
	}
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	// Upgrade HTTP request to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	// Get room ID from path
	pathSegments := strings.Split(r.URL.Path, "/")
	roomID := pathSegments[len(pathSegments)-1]
	name := r.URL.Query().Get("name")

	// Get or create the room
	room, err := roomManager.GetOrCreateRoom(roomID)
	if err != nil {
		log.Println("Room creation error:", err)
		conn.Close()
		return
	}

	// Create a new client
	client, err := room.NewClient(&UserMetadata{
		Name: name,
	})
	if err != nil {
		log.Println("Client creation error:", err)
		conn.Close()
		return
	}

	// Handle incoming messages from WebSocket
	go func() {
		defer func() {
			room.RemoveClient(client)
			conn.Close()
		}()

		for {
			select {
			case <-client.Context().Done():
				return
			default:
				// Read the raw message
				_, rawMsg, err := conn.ReadMessage()
				if err != nil {
					log.Println("Read error:", err)
					return
				}

				msg, err := parseWebSocketMessage(rawMsg)
				if err != nil {
					log.Printf("Message parse error: %v", err)
					continue
				}

				room.HandleClientData(client, msg)
			}
		}
	}()

	// Handle outgoing messages to WebSocket
	go func() {
		defer conn.Close()
		for msg := range client.Receive() {
			data, err := formatWebSocketMessage(msg)
			if err != nil {
				log.Printf("Message format error: %v", err)
				continue
			}

			err = conn.WriteMessage(websocket.TextMessage, data)
			if err != nil {
				log.Println("Write error:", err)
				return
			}
		}
	}()
}

// roomInit initializes a new room with the given ID
func roomInit(ctx context.Context, id string) (*RoomMetadata, error) {
	// Initialization code (e.g., load data from DB)
	return &RoomMetadata{
		Name: "Test",
	}, nil
}

// roomHandler handles all room events and message broadcasting
func roomHandler(ctx context.Context, room *hotel.Room[RoomMetadata, UserMetadata, hotel.Message]) {
	log.Printf("Room %s started", room.ID())

	for {
		select {
		case event := <-room.Events():
			switch event.Type {
			case hotel.EventJoin:
				// A client joined the room.
				name := event.Client.Metadata().Name
				log.Printf("%s joined room %s", name, room.ID())
				room.BroadcastExcept(event.Client, &JoinMessage{Name: name})
			case hotel.EventLeave:
				// A client left the room.
				name := event.Client.Metadata().Name
				log.Printf("%s left room %s", name, room.ID())
				room.BroadcastExcept(event.Client, &LeaveMessage{Name: name})
			case hotel.EventCustom:
				// Incoming message from a client.
				switch msg := event.Data.(type) {
				case *ChatMessage:
					name := event.Client.Metadata().Name
					log.Printf("<%s> in %s: %s", name, room.ID(), msg.Content)
					// Use authenticated name instead of client-provided name
					room.BroadcastExcept(event.Client, &ChatMessage{
						Name:    name,
						Content: msg.Content,
					})
				default:
					log.Printf("Unhandled message type: %T", msg)
				}
			}
		case <-ctx.Done():
			// Handler context canceled, perform cleanup
			log.Printf("Handler for room %s is exiting", room.ID())
			return
		}
	}
}

// formatWebSocketMessage formats a message for websocket transmission
func formatWebSocketMessage(msg hotel.Message) ([]byte, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %v", err)
	}
	return []byte(fmt.Sprintf("%s %s", msg.Type(), string(payload))), nil
}

// parseWebSocketMessage parses a websocket message into a hotel.Message
func parseWebSocketMessage(data []byte) (hotel.Message, error) {
	parts := strings.SplitN(string(data), " ", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid message format: %s", string(data))
	}

	msgType := parts[0]
	payload := []byte(parts[1])

	msg, err := messageRegistry.Create(msgType)
	if err != nil {
		return nil, fmt.Errorf("message creation error: %v", err)
	}

	if err := json.Unmarshal(payload, msg); err != nil {
		return nil, fmt.Errorf("unmarshal error: %v", err)
	}

	return msg, nil
}
