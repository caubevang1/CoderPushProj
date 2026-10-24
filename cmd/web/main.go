// Command web is a browser front end for the catalog, made so reviewers can
// try every scenario of the CLI from a deployed URL. It embeds the fake
// supplier, so a MySQL database is all it needs.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"CoderPush/internal/store"
)

const defaultDSN = "admin:admin@tcp(127.0.0.1:3307)/coderpush_db?parseTime=true&loc=UTC&charset=utf8mb4"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	port := env("PORT", "8080")

	// Hosted MySQL (Aiven) signs its certificate with its own CA: the DSN
	// then says tls=custom and the CA comes in this variable.
	if ca := os.Getenv("CATALOG_DB_CA"); ca != "" {
		if err := store.RegisterCA("custom", []byte(ca)); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := store.Open(ctx, env("CATALOG_DSN", defaultDSN))
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return err
	}

	srv, err := newServer(s, env("FEEDS_DIR", "testdata/feeds"), "http://127.0.0.1:"+port)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- httpServer.ListenAndServe() }()
	log.Printf("catalog web demo listening on :%s", port)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// A redeploy sends SIGTERM: stop the import after the batch in hand, as
	// Ctrl+C does in the CLI, so the run is recorded as interrupted.
	log.Print("shutting down")
	srv.stopImport()
	srv.waitIdle()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
