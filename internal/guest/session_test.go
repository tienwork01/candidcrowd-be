package guest

import "testing"

func TestSessionTableName(t *testing.T) {
	if got, want := (Session{}).TableName(), "guest_sessions"; got != want {
		t.Fatalf("TableName() = %q, want %q", got, want)
	}
}
