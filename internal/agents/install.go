package agents

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Scope selects where an agent integration is installed: per-repository (the
// project's own config files) or per-user (the agent's global config).
type Scope int

const (
	ScopeRepo Scope = iota
	ScopeUser
)

func (s Scope) String() string {
	if s == ScopeUser {
		return "user"
	}
	return "repo"
}

// Paths resolves config locations for an install. Dir is the repo root; Home is
// the user's home directory (used by ScopeUser targets).
type Paths struct {
	Scope Scope
	Dir   string
	Home  string
}

// RepoFile joins parts onto the repo root.
func (p Paths) RepoFile(parts ...string) string {
	return filepath.Join(append([]string{p.Dir}, parts...)...)
}

// UserFile joins parts onto the user home.
func (p Paths) UserFile(parts ...string) string {
	return filepath.Join(append([]string{p.Home}, parts...)...)
}

// InstallResult summarizes one agent's install for human-readable CLI output.
type InstallResult struct {
	Agent string
	MCP   string // "configured" | "already present"
	Hooks string // "written" | "plugin written" | "installed N" | ...
	Docs  string // "written"
}

// Supported is the ordered set of agent names recarto can integrate with, each
// paired with the executable it looks for on PATH during auto-detection.
var Supported = []struct{ Name, Exe string }{
	{"claude", "claude"},
	{"opencode", "opencode"},
	{"copilot", "copilot"},
}

// Detect returns the supported agent names whose CLI is found on PATH.
func Detect() []string {
	var found []string
	for _, s := range Supported {
		if _, err := exec.LookPath(s.Exe); err == nil {
			found = append(found, s.Name)
		}
	}
	return found
}

// HomeDir returns the current user's home directory.
func HomeDir() (string, error) { return os.UserHomeDir() }
