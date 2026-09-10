package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yunzaixi-dev/tjucli/internal/toolserver"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) (runErr error) {
	fs := flag.NewFlagSet("tjucli-server", flag.ContinueOnError)

	defaultAddr := os.Getenv("HTTP_ADDR")
	if defaultAddr == "" {
		defaultAddr = toolserver.DefaultAddr
	}

	defaultGrantsFile := os.Getenv("TJUCLI_GRANTS_FILE")

	addr := fs.String("addr", defaultAddr, "HTTP listen address (env HTTP_ADDR)")
	grantsFile := fs.String("grants-file", defaultGrantsFile, "Path to grants JSON file (env TJUCLI_GRANTS_FILE)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *grantsFile == "" {
		return errors.New("grants file must be specified via -grants-file or TJUCLI_GRANTS_FILE")
	}

	// Validate grants file before starting
	if err := toolserver.ValidateGrantsFile(*grantsFile); err != nil {
		return fmt.Errorf("invalid grants file: %w", err)
	}

	server, err := toolserver.NewServer(toolserver.ServerConfig{
		Addr:           *addr,
		GrantsFilePath: *grantsFile,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize server: %w", err)
	}
	defer func() {
		if runErr != nil {
			_ = server.Close()
		}
	}()

	// Signal handling for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		runErr = fmt.Errorf("server error: %w", err)
		return runErr
	case <-ctx.Done():
		// Received SIGINT or SIGTERM
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			runErr = fmt.Errorf("shutdown error: %w", err)
			return runErr
		}
	}

	return nil
}
