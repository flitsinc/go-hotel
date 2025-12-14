package hotel

import (
	"strings"
	"testing"
)

// TestEventTypeString verifies all EventType values have string representations.
func TestEventTypeString(t *testing.T) {
	tests := []struct {
		eventType EventType
		expected  string
	}{
		{EventJoin, "EventJoin"},
		{EventLeave, "EventLeave"},
		{EventCustom, "EventCustom"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := tt.eventType.String()
			if got != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, got)
			}
		})
	}
}

// TestEventTypeStringUnknown verifies unknown EventType values are handled.
func TestEventTypeStringUnknown(t *testing.T) {
	unknown := EventType(999)
	got := unknown.String()

	if !strings.Contains(got, "999") {
		t.Errorf("Expected string to contain '999', got %q", got)
	}
	if !strings.HasPrefix(got, "<!EventType") {
		t.Errorf("Expected string to start with '<!EventType', got %q", got)
	}
}
