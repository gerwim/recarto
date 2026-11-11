package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/gerwim/recarto/internal/app"
)

// todoListLimit caps `recarto todo list` output.
const todoListLimit = 50

// Todo dispatches `todo list` and `todo done <id>`.
//
//	recarto todo list            # list active todos for the current branch
//	recarto todo done <id>       # mark a todo done
func Todo(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("todo: subcommand required (list|done)")
	}
	switch args[0] {
	case "list":
		return todoList(ctx, a, w)
	case "done":
		return todoDone(ctx, a, w, args[1:])
	default:
		return fmt.Errorf("todo: unknown subcommand %q (list|done)", args[0])
	}
}

func todoList(ctx context.Context, a *app.App, w io.Writer) error {
	todos, err := a.Engine.ActiveTodos(ctx, a.Git.Branch, todoListLimit)
	if err != nil {
		return err
	}
	if len(todos) == 0 {
		fmt.Fprintln(w, "(no open todos)")
		return nil
	}
	for _, r := range todos {
		title := r.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "• %s  %s\n", r.ID, title)
	}
	return nil
}

func todoDone(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("todo done: id required")
	}
	ok, err := a.Engine.CompleteTodo(ctx, args[0])
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(w, "no active todo with id %s\n", args[0])
		return nil
	}
	fmt.Fprintf(w, "done %s\n", args[0])
	return nil
}
