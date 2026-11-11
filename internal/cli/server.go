package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gerwim/recarto/internal/syncserver"
)

// ServerCmd runs the HTTP memory-sync server (`recarto server`) until
// interrupted. Unlike every other command it needs no git context and no
// per-repo App: it is a standalone daemon holding one store per repo key,
// serving whichever repos its authorized clients push/pull. See
// internal/syncserver for the request handling.
func ServerCmd(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dataDir := fs.String("data-dir", "", "per-repo store directory (default: $RECARTO_SERVER_DATA, or an OS data dir)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (optional; omit to run plain HTTP behind a reverse proxy)")
	tlsKey := fs.String("tls-key", "", "TLS private key file")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	key := os.Getenv("RECARTO_SERVER_KEY")
	if key == "" {
		return errors.New("recarto server: RECARTO_SERVER_KEY must be set (a static bearer token shared by every client)")
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return errors.New("recarto server: --tls-cert and --tls-key must be given together")
	}

	dir := *dataDir
	if dir == "" {
		dir = os.Getenv("RECARTO_SERVER_DATA")
	}
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(base, "recarto-server")
	}

	srv := syncserver.New(key, dir)
	defer func() { _ = srv.Close() }()

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}

	sctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		if *tlsCert != "" {
			errCh <- httpSrv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()

	fmt.Fprintf(out, "recarto server listening on %s (data-dir=%s)\n", *addr, dir)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-sctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}
