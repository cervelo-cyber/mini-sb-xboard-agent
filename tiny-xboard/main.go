package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"
)

// Build-time version metadata, injected via build.sh ldflags:
//   go build -ldflags "-X main.version=... -X main.commit=... -X main.buildTime=..."
// Defaults keep plain `go build` working (no build-time inputs required).
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

type Server struct {
	store *Store
}

func main() {
	// Hardening for 64MB-class containers: cap GC threads, and give the GC a
	// soft memory ceiling so heap pressure is released before OOM. Explicitly
	// calling debug.SetMemoryLimit makes the ceiling deterministic even when
	// GOMEMLIMIT is not exported in the container env.
	runtime.GOMAXPROCS(1)
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(32 << 20)
	}

	dir := flag.String("dir", "/etc/tiny-xboard", "state directory (node.json/users.json/traffic.json)")
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("tiny-xboard version=%s commit=%s built=%s go=%s\n", version, commit, buildTime, runtime.Version())
		return
	}

	if err := run(*dir, *listen); err != nil {
		log.Fatal(err)
	}
}

func run(dir, listen string) error {
	store, err := NewStore(dir)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("tiny-xboard api listening on %s (data dir %s)", listen, dir)
	return serve(ctx, store, ln)
}

// serve runs the HTTP server and the traffic flusher until ctx is cancelled,
// then gracefully drains in-flight requests and flushes traffic before exit.
func serve(ctx context.Context, store *Store, ln net.Listener) error {
	srv := &http.Server{
		Handler:           (&Server{store: store}).routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go runTrafficFlusher(ctx, store)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	log.Println("shutting down: draining in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	log.Println("flushing traffic")
	store.flushTrafficIfDirty()
	return nil
}

// runTrafficFlusher periodically persists cumulative traffic when dirty.
func runTrafficFlusher(ctx context.Context, store *Store) {
	interval := time.Duration(store.RuntimeFlushInterval()) * time.Second
	if interval <= 0 {
		interval = time.Duration(DefaultTrafficFlushInterval) * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			store.flushTrafficIfDirty()
		}
	}
}
