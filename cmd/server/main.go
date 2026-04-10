package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/sairamapuroop/PayFlowMock/internal/handler"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
	"github.com/sairamapuroop/PayFlowMock/internal/service"
	"github.com/sairamapuroop/PayFlowMock/pkg/logger"
)

func main() {
	logger.Setup("payflow")

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal().Msg("DATABASE_URL is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	migrationsDir := os.Getenv("MIGRATIONS_PATH")
	if migrationsDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatal().Err(err).Msg("get working directory")
		}
		migrationsDir = filepath.Join(wd, "migrations")
	}

	migrationsURL, err := migrationsFileURL(migrationsDir)
	if err != nil {
		log.Fatal().Err(err).Str("dir", migrationsDir).Msg("resolve migrations path")
	}

	if err := runMigrations(databaseURL, migrationsURL); err != nil {
		log.Fatal().Err(err).Msg("database migrations")
	}

	ctx := context.Background()
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("parse DATABASE_URL for pool")
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("create pgx pool")
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatal().Err(err).Msg("ping database")
	}

	repo := repository.NewPaymentRepo(pool)
	svc := service.NewPaymentService(repo)
	h := handler.NewPaymentHandler(svc, pool)

	r := chi.NewRouter()
	h.Register(r)

	addr := ":" + port
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info().Str("addr", addr).Msg("http server listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("http server")
		}
	}()

	<-shutdownCtx.Done()
	log.Info().Msg("shutting down")

	cancelCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(cancelCtx); err != nil {
		log.Error().Err(err).Msg("http shutdown")
	}
}

func migrationsFileURL(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// migrate file source expects a URL; absolute POSIX path after file:///
	return fmt.Sprintf("file://%s", filepath.ToSlash(abs)), nil
}

func runMigrations(databaseURL, migrationsURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open sql for migrations: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping for migrations: %w", err)
	}

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("migrate pgx driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(migrationsURL, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("migrate instance: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}
