package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/mattn/go-isatty"

	"git.gerwim.nl/gerwim/recarto/internal/features"
)

// Config is `recarto config`: the interactive feature list on a terminal,
// else the plain list. list, enable, disable and path work anywhere.
func Config(out io.Writer, args []string) error {
	path, err := features.Path()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if out == io.Writer(os.Stdout) && isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) {
			return features.Edit(path, os.Stdin, os.Stdout)
		}
		args = []string{"list"}
	}
	switch sub, names := args[0], args[1:]; sub {
	case "list":
		return configList(out, path)
	case "path":
		fmt.Fprintln(out, path)
		return nil
	case "enable", "disable":
		if len(names) == 0 {
			return fmt.Errorf("config %s: name a feature (recarto config list shows them)", sub)
		}
		changes := features.Set{}
		for _, n := range names {
			f, err := features.Lookup(n)
			if err != nil {
				return fmt.Errorf("config %s: %w", sub, err)
			}
			changes[f] = sub == "enable"
		}
		if err := features.Save(path, changes); err != nil {
			return err
		}
		for _, f := range features.All() {
			if on, ok := changes[f]; ok {
				fmt.Fprintf(out, "%s: %s\n", f.Key, features.OnOff(on))
			}
		}
		return nil
	}
	return fmt.Errorf("config: unknown subcommand %q (list|enable|disable|path)", args[0])
}

// configList prints every feature with its state, then the config file's
// problems, if any, as the error.
func configList(out io.Writer, path string) error {
	s, err := features.Read(path)
	width := 0
	for _, f := range features.All() {
		width = max(width, len(f.Key))
	}
	fmt.Fprintln(out, path)
	group := ""
	for _, f := range features.All() {
		if f.Group != group {
			group = f.Group
			fmt.Fprintf(out, "\n%s\n", group)
		}
		fmt.Fprintf(out, "  %-3s  %-*s  %s\n", features.OnOff(s.On(f)), width, f.Key, f.Desc)
	}
	fmt.Fprintln(out, "\nSwitch with recarto config enable|disable <feature>, or run recarto config in a terminal.")
	return err
}
