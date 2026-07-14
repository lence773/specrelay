package app

import (
	"testing"

	"github.com/lyming99/specrelay/backend/internal/domain"
)

func TestRequiresExclusiveWorkspace(t *testing.T) {
	tests := []struct {
		jobType string
		want    bool
	}{
		{jobType: "plan.generate", want: false},
		{jobType: "task.execute", want: true},
		{jobType: "unknown", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.jobType, func(t *testing.T) {
			if got := RequiresExclusiveWorkspace(domain.Job{Type: tt.jobType}); got != tt.want {
				t.Fatalf("RequiresExclusiveWorkspace(%q)=%t, want %t", tt.jobType, got, tt.want)
			}
		})
	}
}
