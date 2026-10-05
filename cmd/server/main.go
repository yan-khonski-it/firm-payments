package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"firm-payments/internal/api"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// No payment service yet: the stub handler does not call it.
	router := api.NewRouter(nil)

	srv := &http.Server{
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Bind before logging, so a busy port fails here instead of after "listening".
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen on %s: %v", addr, err)
	}
	log.Printf("listening on %s", ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	// Restore default signal handling, so a second Ctrl+C exits immediately
	// instead of waiting for the graceful shutdown below.
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
