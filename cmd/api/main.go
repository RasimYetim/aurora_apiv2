package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/rasim/aurora/internal/httpapi"
	"github.com/rasim/aurora/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	_ = godotenv.Load()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL3"))
	if err != nil {
		return err
	}
	defer pool.Close()

	st := &store.Store{Pool: pool}

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = st.ReleaseExpiredCarts(context.Background())
			case <-ctx.Done():
				return
			}
		}
	}()

	srv := &http.Server{
		Addr:    ":" + cmp.Or(os.Getenv("PORT"), "8080"),
		Handler: httpapi.NewRouter(st),
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen", "err", err)
		}
	}()
	<-ctx.Done()
	sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sc)
}
