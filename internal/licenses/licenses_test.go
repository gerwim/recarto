package licenses

import (
	"bytes"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gerwim/recarto"
	"github.com/gerwim/recarto/internal/embedding"
	"github.com/gerwim/recarto/internal/version"
)

func TestWriteStartsWithRecartoAndListsBuiltInComponents(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	rule := strings.Repeat("-", 80) + "\n"
	if !strings.HasPrefix(out, rule+"Recarto "+version.Version+"\nLicense: MIT\n") {
		t.Errorf("output does not start with Recarto's MIT licence:\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, strings.TrimSpace(recarto.License)) {
		t.Error("output does not contain LICENSE")
	}
	for _, want := range []string{
		"Go standard library and runtime " + runtime.Version(),
		"musl libc 1.2.5",
		"MinGW-w64 runtime 12.0.0",
		"potion-base-8M model (MinishLab) " + embedding.Revision,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q", want)
		}
	}
}

func TestToolchainNoticesMatchReleaseBuilder(t *testing.T) {
	data, err := os.ReadFile("../../ci/builder.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`ARG ZIG_VERSION=(\S+)`).FindSubmatch(data)
	if m == nil {
		t.Fatal("ci/builder.Dockerfile has no ARG ZIG_VERSION")
	}
	if got := string(m[1]); got != zigVersion {
		t.Fatalf("Dockerfile Zig %s vs notices from Zig %s: copy lib/libc/musl/COPYRIGHT, lib/libc/mingw/COPYING and LICENSE from that Zig release into internal/licenses/texts and update the versions in licenses.go", got, zigVersion)
	}
}
