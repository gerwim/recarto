package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestImportRequiresExactlyOneArg(t *testing.T) {
	var out bytes.Buffer
	if err := Import(context.Background(), &out, nil); err == nil {
		t.Fatal("expected an error with no archive path given")
	}
	if err := Import(context.Background(), &out, []string{"a.ndjson.gz", "b.ndjson.gz"}); err == nil {
		t.Fatal("expected an error with more than one archive path given")
	}
}

func TestImportRequiresSyncConfig(t *testing.T) {
	t.Setenv("RECARTO_SYNC_URL", "")
	t.Setenv("RECARTO_SYNC_KEY", "")

	path := filepath.Join(t.TempDir(), "archive.ndjson.gz")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Import(context.Background(), &out, []string{path})
	if err == nil {
		t.Fatal("expected an error when RECARTO_SYNC_URL/KEY are unset and no --url/--key given")
	}
}

func TestImportRejectsMissingFile(t *testing.T) {
	t.Setenv("RECARTO_SYNC_URL", "http://example.invalid")
	t.Setenv("RECARTO_SYNC_KEY", "s3cret")

	var out bytes.Buffer
	err := Import(context.Background(), &out, []string{filepath.Join(t.TempDir(), "does-not-exist.ndjson.gz")})
	if err == nil {
		t.Fatal("expected an error for a missing archive file")
	}
}

// TestImportFlagsMustPrecedeFile pins the documented invocation shape
// (`recarto import [--url --key] <file>`, matching the usage string and the
// Import doc comment): Go's flag.Parse stops at the first non-flag
// argument, so flags placed AFTER the file path are treated as extra
// positional args and rejected, not silently accepted.
func TestImportFlagsMustPrecedeFile(t *testing.T) {
	t.Setenv("RECARTO_SYNC_URL", "")
	t.Setenv("RECARTO_SYNC_KEY", "")
	path := filepath.Join(t.TempDir(), "archive.ndjson.gz")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Import(context.Background(), &out, []string{path, "--url", "http://example.invalid", "--key", "s3cret"})
	if err == nil {
		t.Fatal("expected flags placed after the file path to be rejected as extra positional args")
	}
}
