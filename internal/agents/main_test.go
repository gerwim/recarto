package agents

import (
	"context"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Tests end each wait themselves (endWaitWhen); the bound only catches a hang.
	firstIndexDeadline = func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, hang)
	}
	os.Exit(m.Run())
}
