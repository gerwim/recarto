package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
	"git.gerwim.nl/gerwim/recarto/internal/agents/claudecode"
	"git.gerwim.nl/gerwim/recarto/internal/agents/copilot"
	"git.gerwim.nl/gerwim/recarto/internal/agents/opencode"
)

// Init configures Recarto integration for one or more agents. With no --agent it
// auto-detects installed agents (falling back to claude); --scope chooses repo
// (default) or user. It registers the recarto binary as `binary` and is idempotent.
func Init(out io.Writer, dir, binary string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	agentList := fs.String("agent", "", "comma list: claude,opencode,copilot (default: auto-detect)")
	scopeStr := fs.String("scope", "", "user|repo (default: repo)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	selected := splitCSV(*agentList)
	if len(selected) == 0 {
		selected = agents.Detect()
		if len(selected) == 0 {
			selected = []string{"claude"} // sensible fallback when nothing detected
		}
	}

	var scope agents.Scope
	switch *scopeStr {
	case "user":
		scope = agents.ScopeUser
	case "repo", "":
		scope = agents.ScopeRepo
	default:
		return fmt.Errorf("init: invalid --scope %q (user|repo)", *scopeStr)
	}

	home, err := agents.HomeDir()
	if err != nil {
		return err
	}
	p := agents.Paths{Scope: scope, Dir: dir, Home: home}

	fmt.Fprintf(out, "recarto init (scope=%s, agents=%s)\n", scope, strings.Join(selected, ","))
	for _, name := range selected {
		res, err := installAgent(name, p, binary)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  %-9s mcp:%s hooks:%s docs:%s\n", res.Agent, res.MCP, res.Hooks, res.Docs)
	}
	return nil
}

// installAgent runs the installer for one agent host name.
func installAgent(name string, p agents.Paths, binary string) (agents.InstallResult, error) {
	switch name {
	case "claude", "claude-code":
		return claudecode.Install(p, binary)
	case "opencode":
		return opencode.Install(p, binary)
	case "copilot":
		return copilot.Install(p, binary)
	}
	return agents.InstallResult{}, fmt.Errorf("init: unknown agent %q (claude|opencode|copilot)", name)
}
