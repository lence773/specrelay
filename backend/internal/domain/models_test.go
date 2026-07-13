package domain

import "testing"

func TestStateTransitions(t *testing.T) {
	if err := ValidateTransition("task", "pending", "queued"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransition("task", "pending", "succeeded"); err == nil {
		t.Fatal("expected invalid transition")
	}
	if err := ValidateTransition("job", "running", "retry_wait"); err != nil {
		t.Fatal(err)
	}
}
