package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
)

// Import uploads a previously written export archive (see Export /
// `recarto export`) to the sync server's bulk POST /v1/import endpoint. It is
// the other half of `recarto export --push`, for when the archive is
// uploaded from a different machine than the one that wrote it, or uploaded
// later rather than in the same command.
func Import(ctx context.Context, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	url := fs.String("url", "", "sync server URL (default: $RECARTO_SYNC_URL)")
	key := fs.String("key", "", "sync server key (default: $RECARTO_SYNC_KEY)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: recarto import <archive.ndjson.gz> [--url --key]")
	}

	client, err := importClient(*url, *key)
	if err != nil {
		return err
	}
	f, err := os.Open(rest[0])
	if err != nil {
		return err
	}
	defer f.Close()

	resp, err := client.Import(ctx, f)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "import: %d/%d records accepted across %d repo(s)\n",
		resp.TotalAccepted, resp.TotalEntries, len(resp.Repos))
	return nil
}
