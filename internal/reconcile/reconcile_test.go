package reconcile

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestLowestRequeuingResult(t *testing.T) {
	tests := []struct {
		name       string
		i          Result
		j          Result
		wantResult Result
	}{
		{"bail,requeue", ResultEmpty, ResultRequeue, ResultRequeue},
		{"bail,requeueInterval", ResultEmpty, ResultSuccess, ResultSuccess},
		{"requeue,bail", ResultRequeue, ResultEmpty, ResultRequeue},
		{"requeue,requeueInterval", ResultRequeue, ResultSuccess, ResultRequeue},
		{"requeueInterval,requeue", ResultSuccess, ResultRequeue, ResultRequeue},
		{"requeueInterval,requeueInterval", ResultSuccess, ResultSuccess, ResultSuccess},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			g.Expect(LowestRequeuingResult(tt.i, tt.j)).To(Equal(tt.wantResult))
		})
	}
}
