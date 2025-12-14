package hotel

import (
	"testing"
)

// TestMessage is a simple message type for testing
type TestMessage struct {
	Content string
}

func (m *TestMessage) Type() string {
	return "test"
}

// AnotherMessage is another message type for testing
type AnotherMessage struct {
	Value int
}

func (m *AnotherMessage) Type() string {
	return "another"
}

// TestMessageRegistryRegister verifies that messages can be registered.
func TestMessageRegistryRegister(t *testing.T) {
	registry := make(MessageRegistry[Message])

	registry.Register(&TestMessage{})

	if len(registry) != 1 {
		t.Errorf("Expected 1 registered type, got %d", len(registry))
	}

	if _, ok := registry["test"]; !ok {
		t.Error("Expected 'test' type to be registered")
	}
}

// TestMessageRegistryRegisterMultiple verifies multiple messages can be registered.
func TestMessageRegistryRegisterMultiple(t *testing.T) {
	registry := make(MessageRegistry[Message])

	registry.Register(&TestMessage{}, &AnotherMessage{})

	if len(registry) != 2 {
		t.Errorf("Expected 2 registered types, got %d", len(registry))
	}

	if _, ok := registry["test"]; !ok {
		t.Error("Expected 'test' type to be registered")
	}
	if _, ok := registry["another"]; !ok {
		t.Error("Expected 'another' type to be registered")
	}
}

// TestMessageRegistryRegisterDuplicate verifies that duplicate registration panics.
func TestMessageRegistryRegisterDuplicate(t *testing.T) {
	registry := make(MessageRegistry[Message])

	registry.Register(&TestMessage{})

	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected panic on duplicate registration")
		}
	}()

	// This should panic
	registry.Register(&TestMessage{})
}

// TestMessageRegistryCreate verifies that registered messages can be created.
func TestMessageRegistryCreate(t *testing.T) {
	registry := make(MessageRegistry[Message])
	registry.Register(&TestMessage{})

	msg, err := registry.Create("test")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if msg == nil {
		t.Fatal("Expected non-nil message")
	}

	if msg.Type() != "test" {
		t.Errorf("Expected type 'test', got %q", msg.Type())
	}

	// Verify it's actually a *TestMessage
	testMsg, ok := msg.(*TestMessage)
	if !ok {
		t.Error("Expected message to be *TestMessage")
	}

	// Verify it's a new instance (not the same as registered)
	if testMsg.Content != "" {
		t.Error("Expected empty Content for new instance")
	}
}

// TestMessageRegistryCreateUnknown verifies that creating unknown types returns error.
func TestMessageRegistryCreateUnknown(t *testing.T) {
	registry := make(MessageRegistry[Message])

	_, err := registry.Create("unknown")
	if err == nil {
		t.Error("Expected error for unknown message type")
	}
}

// TestMessageRegistryCreateMultipleTypes verifies creating different types works.
func TestMessageRegistryCreateMultipleTypes(t *testing.T) {
	registry := make(MessageRegistry[Message])
	registry.Register(&TestMessage{}, &AnotherMessage{})

	msg1, err := registry.Create("test")
	if err != nil {
		t.Fatalf("Create 'test' failed: %v", err)
	}
	if _, ok := msg1.(*TestMessage); !ok {
		t.Error("Expected *TestMessage")
	}

	msg2, err := registry.Create("another")
	if err != nil {
		t.Fatalf("Create 'another' failed: %v", err)
	}
	if _, ok := msg2.(*AnotherMessage); !ok {
		t.Error("Expected *AnotherMessage")
	}
}
