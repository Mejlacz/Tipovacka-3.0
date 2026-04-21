// db/db.go — Tipovačka 2.0
// PostgreSQL connection pool via pgx/v5.
package db

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
	"tipovacka/config"
)

var Pool *pgxpool.Pool

func Init() {
	if config.DatabaseURL == "" {
		log.Fatal("[DB] DATABASE_URL není nastavena")
	}
	var err error
	Pool, err = pgxpool.New(context.Background(), config.DatabaseURL)
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
