// db/db.go — Tipovačka 2.0
// PostgreSQL connection pool via pgx/v5.
package db

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"tipovacka/config"
)

var Pool *pgxpool.Pool

func Init() {
	if config.DatabaseURL == "" {
		log.Fatal("[DB] DATABASE_URL není nastavena")
	}
	cfg, err := pgxpool.ParseConfig(config.DatabaseURL)
	if err != nil {
		log.Fatalf("[DB] Nelze parsovat DATABASE_URL: %v", err)
	}
	// CockroachDB Serverless — omez RU spotřebu poolu
	cfg.MaxConns = 4
	cfg.MinConns = 1
	cfg.HealthCheckPeriod = 60 * time.Minute // default 5 min → každý check = RU
	cfg.MaxConnIdleTime = 10 * time.Minute   // uvolni neaktivní spojení

	Pool, err = pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatalf("[DB] Nelze vytvořit pool: %v", err)
	}
	if err = Pool.Ping(context.Background()); err != nil {
		log.Fatalf("[DB] Ping selhal: %v", err)
	}
	fmt.Println("[DB] Připojeno k PostgreSQL")
}

func Close() {
	if Pool != nil {
		Pool.Close()
	}
}
