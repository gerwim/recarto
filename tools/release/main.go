// Command release builds/merges the recarto distribution manifest for CI. It
// hashes the freshly built binaries, inserts a new version into the existing
// index.json (fetched from R2 beforehand, or absent on first release), and writes
// the updated index.json. CI signs it with minisign and uploads it to R2.
//
// Usage:
//
//	release -version 1.5.0 -released 2026-06-07T12:00:00Z \
//	  -in index.json -out index.json \
//	  -artifact linux-amd64=releases/linux-amd64/recarto:recarto/1.5.0/recarto-linux-amd64 \
//	  -artifact darwin-arm64=releases/darwin-arm64/recarto:recarto/1.5.0/recarto-darwin-arm64 \
//	  -artifact windows-amd64=releases/windows-amd64/recarto.exe:recarto/1.5.0/recarto-windows-amd64.exe
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/gerwim/recarto/internal/update"
)

type artifactInput struct {
	Platform string // linux-amd64
	File     string // local path to the built binary
	RPath    string // path under the R2 base, recorded in the manifest
}

type artifactFlags []artifactInput

func (a *artifactFlags) String() string { return "" }
func (a *artifactFlags) Set(v string) error {
	// platform=localfile:rpath
	eq := strings.IndexByte(v, '=')
	colon := strings.LastIndexByte(v, ':')
	if eq < 0 || colon < 0 || colon < eq {
		return fmt.Errorf("artifact must be platform=localfile:rpath, got %q", v)
	}
	*a = append(*a, artifactInput{Platform: v[:eq], File: v[eq+1 : colon], RPath: v[colon+1:]})
	return nil
}

func buildRelease(version, released string, inputs []artifactInput) (update.Release, error) {
	rel := update.Release{Version: version, Released: released, Artifacts: map[string]update.Artifact{}}
	for _, in := range inputs {
		data, err := os.ReadFile(in.File)
		if err != nil {
			return update.Release{}, fmt.Errorf("read %s: %w", in.File, err)
		}
		sum := sha256.Sum256(data)
		rel.Artifacts[in.Platform] = update.Artifact{
			Path:   in.RPath,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(data)),
		}
	}
	return rel, nil
}

func loadManifest(path string) (*update.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // first release
		}
		return nil, err
	}
	return update.Parse(data)
}

func main() {
	var (
		version  = flag.String("version", "", "release version, e.g. 1.5.0")
		released = flag.String("released", "", "RFC3339 release timestamp")
		in       = flag.String("in", "", "existing index.json (optional)")
		out      = flag.String("out", "index.json", "output index.json path")
		arts     artifactFlags
	)
	flag.Var(&arts, "artifact", "platform=localfile:rpath (repeatable)")
	flag.Parse()

	if *version == "" || *released == "" || len(arts) == 0 {
		fmt.Fprintln(os.Stderr, "release: -version, -released and at least one -artifact are required")
		os.Exit(2)
	}

	base, err := loadManifest(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	rel, err := buildRelease(*version, *released, arts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	merged := update.MergeManifest(base, rel)
	data, err := merged.Marshal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (latest=%s, %d versions)\n", *out, merged.Latest, len(merged.Versions))
}
