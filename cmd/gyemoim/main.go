package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gosuda/gyemoim/internal/config"
	"github.com/gosuda/gyemoim/internal/connect"
	"github.com/gosuda/gyemoim/internal/datadir"
	"github.com/gosuda/gyemoim/internal/gateway"
	"github.com/gosuda/gyemoim/internal/history"
	"github.com/gosuda/gyemoim/internal/httpapi"
	"github.com/gosuda/gyemoim/internal/httpui"
	"github.com/gosuda/gyemoim/internal/processlock"
	"github.com/gosuda/gyemoim/internal/provider"
	"github.com/gosuda/gyemoim/internal/siwc"
)

const (
	shutdownTimeout       = 30 * time.Second
	keyUsageFlushInterval = 30 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gyemoim:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("gyemoim", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	port := flags.Int("port", 9092, "TCP port for the WebUI (1-65535); shorthand for --listen 127.0.0.1:PORT")
	listen := flags.String("listen", "", "host:port address to listen on; when both are given, --listen wins over --port")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: gyemoim [--listen HOST:PORT] [--port PORT]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", *port)
	}
	listenHost := "127.0.0.1"
	listenPort := *port
	if *listen != "" {
		host, portText, err := net.SplitHostPort(*listen)
		if err != nil {
			return fmt.Errorf("--listen must be a host:port address, got %q", *listen)
		}
		listenPort, err = strconv.Atoi(portText)
		if err != nil || listenPort < 1 || listenPort > 65535 {
			return fmt.Errorf("--listen port must be between 1 and 65535, got %q", portText)
		}
		listenHost = host
	}

	dataDirectory, err := datadir.Path()
	if err != nil {
		return err
	}
	if err := datadir.Prepare(dataDirectory); err != nil {
		return err
	}
	lock, err := processlock.Acquire(dataDirectory)
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintln(os.Stderr, "gyemoim:", err)
		}
	}()

	databaseContext, cancelDatabaseContext := context.WithTimeout(context.Background(), 30*time.Second)
	store, err := config.Open(databaseContext, dataDirectory)
	cancelDatabaseContext()
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "gyemoim: close configuration database:", err)
		}
	}()

	// Bootstrap runs during startup sequencing (here, not inside the store) so
	// the config store stays a plain data owner. The process lock guarantees a
	// single process per data directory; the store additionally performs the
	// check-and-create in one transaction. The password goes to stderr only —
	// never to history or the database — and is shown exactly once.
	bootstrapContext, cancelBootstrapContext := context.WithTimeout(context.Background(), 30*time.Second)
	bootstrapPassword, bootstrapped, err := store.EnsureBootstrapAdmin(bootstrapContext)
	cancelBootstrapContext()
	if err != nil {
		return err
	}
	if bootstrapped {
		fmt.Fprintf(os.Stderr, "No management users exist. Created initial user %q.\nInitial password (shown once; change it at first login): %s\n", config.BootstrapUsername, bootstrapPassword)
	}

	historyRecorder, historyOpenErr := history.Open(dataDirectory)
	if historyOpenErr != nil {
		fmt.Fprintln(os.Stderr, "gyemoim: request history is unavailable; new inference requests will be rejected")
	}
	defer func() {
		if err := historyRecorder.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "gyemoim: request history did not close cleanly")
		}
	}()

	startedAt := time.Now().UTC()
	// Diagnostics-only in-memory tracker of successful local-key
	// authentications; the gateway records into it on the request path and the
	// periodic flusher below persists it to config.db off the request path.
	keyUsage := gateway.NewKeyUsageTracker()
	uiHandler, err := httpui.New(dataDirectory, listenPort, startedAt, store, historyRecorder)
	if err != nil {
		return err
	}

	oauthManager := siwc.NewManager(store, listenPort)
	connectService := connect.NewService(store, oauthManager)
	responsesAdapter := provider.NewOpenAIResponsesAdapter()
	// Diagnostics-only ring of the last pre-admission harness rejections; the
	// harness records into it and the management API reads it.
	rejectionLog := httpapi.NewRejectionLog()
	mux := http.NewServeMux()
	// /api/ and the UI pages gate themselves on a management session inside
	// their handlers (login and logout opt out; page requests redirect to
	// /login). /auth/callback and /v1/ need no session.
	mux.Handle("/api/", httpapi.NewManagement(store, uiHandler, oauthManager, connectService, historyRecorder, rejectionLog))
	mux.Handle("GET /auth/callback", http.HandlerFunc(oauthManager.ServeCallback))
	// The connect endpoints sit deliberately OUTSIDE the session gate: they
	// serve the enrollment script running on the admin's browser machine,
	// which is not a browser session and cannot carry the session cookie.
	// The single-use enrollment code is the capability instead — 128 bits of
	// randomness, a ~10-minute TTL, consumed at the first claim, bound to one
	// provider. Both endpoints answer every failure with the same generic JSON
	// error and never reveal whether a code existed, expired, or was already
	// used; completing a flow additionally requires the single-use OAuth state
	// recorded at claim time. See docs/web-deployment.md decision 7.
	mux.Handle("POST /connect/claim", http.HandlerFunc(connectService.ServeClaim))
	mux.Handle("POST /connect/complete", http.HandlerFunc(connectService.ServeComplete))
	mux.Handle("/v1/", httpapi.NewHarness(gateway.New(store, keyUsage), historyRecorder, oauthManager, responsesAdapter, rejectionLog))
	mux.Handle("/", httpapi.RequireManagementSession(store, uiHandler))

	// Local-key last-used flusher. The gateway records successful local-key
	// authentications only in memory (constant-time, process-local, like the
	// rejection ring); this goroutine persists a snapshot every 30 s so the
	// request path never writes to config.db. There is no other ticker worker
	// in main to piggyback on — DeleteExpiredSessions runs opportunistically
	// per login request instead — so the flusher owns a plain ticker like the
	// history rotation checker does. Restart semantics: in-memory entries are
	// lost at restart, so a key's stored last-used time can lag successful use
	// by at most one flush interval; that staleness is accepted by design.
	flushStop := make(chan struct{})
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		ticker := time.NewTicker(keyUsageFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				flushKeyUsage(store, keyUsage)
			case <-flushStop:
				return
			}
		}
	}()
	// Registered after store.Close's defer above, so it runs before it: the
	// flusher stops and one final best-effort flush happens before the
	// database connection closes. If this flush fails, up to one flush
	// interval of last-used updates is lost — acceptable for diagnostics data.
	defer func() {
		close(flushStop)
		<-flushDone
		flushKeyUsage(store, keyUsage)
	}()

	serverBase, cancelServerBase := context.WithCancel(context.Background())
	server := &http.Server{
		Addr:              net.JoinHostPort(listenHost, strconv.Itoa(listenPort)),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(net.Listener) context.Context {
			return serverBase
		},
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		cancelServerBase()
		return fmt.Errorf("cannot start at http://%s/ (data directory %q): %w", server.Addr, dataDirectory, err)
	}
	fmt.Fprintf(os.Stderr, "Gyemoim listening at http://%s/\nData directory: %s\n", listener.Addr().(*net.TCPAddr).String(), dataDirectory)

	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			cancelServerBase()
			return nil
		}
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		shutdownErr := server.Shutdown(shutdownContext)
		cancelShutdown()
		if shutdownErr != nil {
			cancelServerBase()
			_ = server.Close()
		} else {
			cancelServerBase()
		}
		return fmt.Errorf("HTTP server stopped: %w", err)
	case <-signals.Done():
		fmt.Fprintln(os.Stderr, "Gyemoim received a shutdown signal; stopping gracefully.")
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		shutdownErr := server.Shutdown(shutdownContext)
		cancelShutdown()
		if shutdownErr != nil {
			fmt.Fprintln(os.Stderr, "Graceful shutdown exceeded 30 seconds; cancelling active requests.")
			cancelServerBase()
			if closeErr := server.Close(); closeErr != nil {
				fmt.Fprintln(os.Stderr, "gyemoim: close active connections:", closeErr)
			}
		} else {
			cancelServerBase()
		}
		serveErr := <-serveResult
		if !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("HTTP server stopped during shutdown: %w", serveErr)
		}
		return nil
	}
}

// flushKeyUsage persists one tracker snapshot off the request path. A failed
// flush re-records its entries into the tracker (Record's forward-only rule
// keeps any newer racing time) so the next interval retries instead of losing
// them; the error is reported on stderr only.
func flushKeyUsage(store *config.Store, keyUsage *gateway.KeyUsageTracker) {
	entries := keyUsage.TakePending()
	if len(entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.UpdateKeysLastUsed(ctx, entries); err != nil {
		for keyID, used := range entries {
			keyUsage.Record(keyID, used)
		}
		fmt.Fprintln(os.Stderr, "gyemoim: flush local key last-used timestamps:", err)
	}
}
