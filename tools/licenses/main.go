// Command licenses generates internal/licenses/modules.txt and
// internal/licenses/inputs.txt. Run it from the repository root after a go.mod
// change or after recarto's code starts importing another third-party package;
// `go test ./...` reports when it must be re-run. It needs network access to
// fetch the modules of other platforms.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/gerwim/recarto/internal/licenses"
)

// Release targets plus macOS (`make darwin` and `go install`); their union is
// noticed in every binary.
var platforms = []string{"linux/amd64", "windows/amd64", "darwin/arm64"}

// SPDX expression per module path, covering every licence file printed for it.
var licenseIDs = map[string]string{
	"aead.dev/minisign":                             "MIT",
	"github.com/bmatcuk/doublestar/v4":              "MIT",
	"github.com/dustin/go-humanize":                 "MIT",
	"github.com/google/jsonschema-go":               "MIT",
	"github.com/google/uuid":                        "BSD-3-Clause",
	"github.com/mattn/go-isatty":                    "MIT",
	"github.com/mattn/go-pointer":                   "MIT",
	"github.com/minio/selfupdate":                   "Apache-2.0 AND MIT AND BSD-3-Clause",
	"github.com/mitchellh/colorstring":              "MIT",
	"github.com/modelcontextprotocol/go-sdk":        "Apache-2.0 AND MIT",
	"github.com/ncruces/go-strftime":                "MIT",
	"github.com/remyoudompheng/bigfft":              "BSD-3-Clause",
	"github.com/rivo/uniseg":                        "MIT",
	"github.com/schollz/progressbar/v3":             "MIT",
	"github.com/segmentio/asm":                      "MIT",
	"github.com/segmentio/encoding":                 "MIT AND Apache-2.0",
	"github.com/tree-sitter/go-tree-sitter":         "MIT AND ICU",
	"github.com/tree-sitter/tree-sitter-c-sharp":    "MIT",
	"github.com/tree-sitter/tree-sitter-go":         "MIT",
	"github.com/tree-sitter/tree-sitter-javascript": "MIT",
	"github.com/tree-sitter/tree-sitter-python":     "MIT",
	"github.com/tree-sitter/tree-sitter-rust":       "MIT",
	"github.com/tree-sitter/tree-sitter-typescript": "MIT",
	"github.com/yosida95/uritemplate/v3":            "BSD-3-Clause",
	"golang.org/x/crypto":                           "BSD-3-Clause",
	"golang.org/x/mod":                              "BSD-3-Clause",
	"golang.org/x/oauth2":                           "BSD-3-Clause",
	"golang.org/x/sys":                              "BSD-3-Clause",
	"golang.org/x/term":                             "BSD-3-Clause",
	"golang.org/x/text":                             "BSD-3-Clause",
	"modernc.org/libc":                              "BSD-3-Clause AND MIT",
	"modernc.org/mathutil":                          "BSD-3-Clause",
	"modernc.org/memory":                            "BSD-3-Clause",
	"modernc.org/sqlite":                            "BSD-3-Clause",
	"mvdan.cc/sh/v3":                                "BSD-3-Clause",
}

func main() {
	if err := run(); err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "licenses: "+format+"\n", args...)
	os.Exit(1)
}

func run() error {
	if _, err := os.ReadFile("go.mod"); err != nil {
		return fmt.Errorf("read go.mod: %w (run from the repository root)", err)
	}

	type module struct {
		Path    string
		Version string
		Dir     string
	}
	seen := map[string]module{}
	for _, platform := range platforms {
		goos, goarch, ok := strings.Cut(platform, "/")
		if !ok {
			return fmt.Errorf("bad platform %q", platform)
		}
		cmd := exec.Command("go", "list", "-deps", "-json=Module", "./cmd/recarto")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=1")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go list for %s: %w\n%s", platform, err, stderr.String())
		}
		dec := json.NewDecoder(&stdout)
		for {
			var pkg struct {
				Module *struct {
					Path    string
					Version string
					Main    bool
					Dir     string
				}
			}
			if err := dec.Decode(&pkg); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return fmt.Errorf("decode go list for %s: %w", platform, err)
			}
			m := pkg.Module
			if m == nil || m.Main || m.Path == "" {
				continue
			}
			seen[m.Path] = module{Path: m.Path, Version: m.Version, Dir: m.Dir}
		}
	}

	for path, mod := range seen {
		if _, ok := licenseIDs[path]; !ok {
			return fmt.Errorf("%s %s has no licence in licenseIDs; add its SPDX licence", path, mod.Version)
		}
	}
	for path := range licenseIDs {
		if _, ok := seen[path]; !ok {
			return fmt.Errorf("%s is in licenseIDs but not in the binary; remove it", path)
		}
	}

	modules := make([]module, 0, len(seen))
	for _, mod := range seen {
		modules = append(modules, mod)
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Path < modules[j].Path })

	var out strings.Builder
	for _, mod := range modules {
		texts, err := licenseFiles(mod.Dir)
		if err != nil {
			return err
		}
		if len(texts) == 0 {
			return fmt.Errorf("%s %s has no licence file", mod.Path, mod.Version)
		}
		out.WriteString(licenses.Notice{
			Name:    mod.Path,
			Version: mod.Version,
			License: licenseIDs[mod.Path],
			Texts:   texts,
		}.Render())
	}
	if err := os.WriteFile("internal/licenses/modules.txt", []byte(out.String()), 0o644); err != nil {
		return err
	}

	in, err := inputs(".")
	if err != nil {
		return err
	}
	return os.WriteFile("internal/licenses/inputs.txt", []byte(in), 0o644)
}

func licenseFiles(dir string) ([]licenses.Text, error) {
	var texts []licenses.Text
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == dir {
				return nil
			}
			name := d.Name()
			if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if !strings.HasPrefix(name, "licen") && !strings.HasPrefix(name, "copying") &&
			!strings.HasPrefix(name, "copyright") && !strings.HasPrefix(name, "notice") {
			return nil
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case "", ".txt", ".md", ".mit":
		default:
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		texts = append(texts, licenses.Text{
			Path: filepath.ToSlash(rel),
			Body: strings.ReplaceAll(string(body), "\r\n", "\n"),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(texts, func(i, j int) bool {
		ni, nj := strings.Contains(texts[i].Path, "/"), strings.Contains(texts[j].Path, "/")
		if ni != nj {
			return !ni
		}
		return texts[i].Path < texts[j].Path
	})
	return texts, nil
}

// inputs records what modules.txt was generated from: go.mod's requirements and
// the third-party packages recarto's own packages import on each release
// platform. It reads only files under root, so a test can recompute it offline.
func inputs(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return "", err
	}
	if f.Module == nil {
		return "", fmt.Errorf("go.mod has no module line")
	}
	module := f.Module.Mod.Path

	var requires []string
	for _, req := range f.Require {
		requires = append(requires, "require "+req.Mod.Path+" "+req.Mod.Version)
	}
	sort.Strings(requires)

	var imports []string
	for _, platform := range platforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		ctx := build.Default
		ctx.GOOS, ctx.GOARCH = goos, goarch
		ctx.CgoEnabled = true

		seen := map[string]bool{}
		imported := map[string]bool{}
		queue := []string{module + "/cmd/recarto"}
		for len(queue) > 0 {
			imp := queue[0]
			queue = queue[1:]
			if seen[imp] {
				continue
			}
			seen[imp] = true
			rel := strings.TrimPrefix(strings.TrimPrefix(imp, module), "/")
			pkg, err := ctx.ImportDir(filepath.Join(root, filepath.FromSlash(rel)), 0)
			if err != nil {
				return "", fmt.Errorf("load %s for %s: %w", imp, platform, err)
			}
			for _, dep := range pkg.Imports {
				if dep == "C" {
					continue
				}
				if dep == module || strings.HasPrefix(dep, module+"/") {
					queue = append(queue, dep)
					continue
				}
				first, _, _ := strings.Cut(dep, "/")
				if strings.Contains(first, ".") && !imported[dep] {
					imported[dep] = true
					imports = append(imports, "import "+platform+" "+dep)
				}
			}
		}
	}
	sort.Strings(imports)

	var b strings.Builder
	b.WriteString("# Inputs internal/licenses/modules.txt was generated from: go.mod's requirements and the third-party packages recarto imports on each release platform.\n")
	b.WriteString("# Generated by go run ./tools/licenses; do not edit.\n")
	for _, line := range requires {
		b.WriteString(line + "\n")
	}
	for _, line := range imports {
		b.WriteString(line + "\n")
	}
	return b.String(), nil
}
