package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/licenses"
)

func TestLicensesNoticeEveryLinkedModule(t *testing.T) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build info")
	}
	var buf bytes.Buffer
	if err := licenses.Write(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, dep := range bi.Deps {
		if !strings.Contains(out, "\n"+dep.Path+" "+dep.Version+"\n") {
			t.Errorf("the binary links %s %s but has no notice: run go run ./tools/licenses from the repository root", dep.Path, dep.Version)
		}
	}
}
