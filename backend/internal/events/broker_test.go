package events

import (
	"context"
	"fmt"
	"testing"
)

func TestExpectedWaitEndRecognizesWrappedContextErrors(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		fmt.Errorf("timeout: %w", context.DeadlineExceeded),
		context.Canceled,
		fmt.Errorf("wait stopped: %w", context.Canceled),
	} {
		if !expectedWaitEnd(err) {
			t.Fatalf("expected context termination error to be recognized: %v", err)
		}
	}
	if expectedWaitEnd(fmt.Errorf("connection closed")) {
		t.Fatal("ordinary connection errors must trigger reconnect logging")
	}
}
