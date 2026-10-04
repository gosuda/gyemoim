package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/gosuda/gyemoim/internal/datadir"
	"github.com/gosuda/gyemoim/internal/gateway"
	"github.com/gosuda/gyemoim/internal/history"
	"github.com/gosuda/gyemoim/internal/httpapi"
	"github.com/gosuda/gyemoim/internal/httpui"
	"github.com/gosuda/gyemoim/internal/processlock"
	"github.com/gosuda/gyemoim/internal/provider"
	"github.com/gosuda/gyemoim/internal/siwc"
	"github.com/gosuda/gyemoim/internal/websecurity"
)

const shutdownTimeout = 30 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gyemoim:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("gyemoim", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	port := flags.Int("port", 9092, "TCP port for the loopback WebUI (1-65535)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: gyemoim [--port PORT]")
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

	historyRecorder, historyOpenErr := history.Open(dataDirectory)
	if historyOpenErr != nil {
		fmt.Fprintln(os.Stderr, "gyemoim: request history is unavailable; new inference requests will be rejected")
	}
	defer func() {
		if err := historyRecorder.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "gyemoim: request history did not close cleanly")
		}
	}()

	csrfToken, err := randomToken()
	if err != nil {
		return fmt.Errorf("create management request token: %w", err)
	}
	startedAt := time.Now().UTC()
	uiHandler, err := httpui.New(dataDirectory, *port, startedAt, csrfToken, store, historyRecorder)
	if err != nil {
		return err
	}

	guard := websecurity.New(*port, csrfToken)
	oauthManager := siwc.NewManager(store, *port)
	responsesAdapter := provider.NewOpenAIResponsesAdapter()
	mux := http.NewServeMux()
	// Browser-origin protections cover the UI and management JSON API. Bearer-authenticated
	// harness routes share only the loopback Host guard applied by the server.
	mux.Handle("/api/", guard.Management(httpapi.NewManagement(store, uiHandler, oauthManager, *port, historyRecorder)))
	mux.Handle("GET /auth/callback", guard.Callback(http.HandlerFunc(oauthManager.ServeCallback)))
	mux.Handle("/v1/", httpapi.NewHarness(gateway.New(store), historyRecorder, oauthManager, responsesAdapter))
	mux.Handle("/", guard.Management(uiHandler))

	serverBase, cancelServerBase := context.WithCancel(context.Background())
	server := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)),
		Handler:           guard.Host(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(net.Listener) context.Context {
			return serverBase
		},
	}
	listener, err := net.Listen("tcp4", server.Addr)
	if err != nil {
		cancelServerBase()
		return fmt.Errorf("cannot start at http://127.0.0.1:%d/ (data directory %q): %w", *port, dataDirectory, err)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "Gyemoim listening at http://127.0.0.1:%d/\nData directory: %s\n", actualPort, dataDirectory)

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

func randomToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
