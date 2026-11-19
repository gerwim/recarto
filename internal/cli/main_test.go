package cli

import (
	"errors"
	"os"
	"testing"

	"github.com/gerwim/recarto/internal/agents"
)

func TestMain(m *testing.M) {
	// A hook run by a test would otherwise start `<test binary> warm`, which
	// reruns the package's tests.
	startWarm = func([]string) error { return nil }
	startIndex = func(string) (<-chan error, error) { return nil, errors.New("tests start no index process") }
	// Tests must not start the host's own `claude`.
	claudeCodeNote = func() string { return "" }
	claudeCodeWarning = func(agents.Paths) string { return "" }
	os.Exit(m.Run())
}
