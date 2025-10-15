package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	// Check DB connectivity once at startup (non-fatal)
	if err := checkDB(); err != nil {
		log.Printf("DB check failed: %v", err)
	} else {
		log.Printf("DB check: connected OK")
	}

	// Run lightweight DB migration to ensure accounts table has token_validated_at
	if err := ensureAccountsMigration(); err != nil {
		log.Printf("DB migration warning: %v", err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("pong: %s", r.RemoteAddr)
	})

	addr := ":3001"
	log.Printf("starting server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// checkDB attempts to open and ping the database using env vars. Returns error if unreachable.
func checkDB() error {
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	if dbUser == "" || dbHost == "" || dbName == "" {
		return fmt.Errorf("missing DB env vars (DB_USER, DB_HOST, DB_NAME required)")
	}
	if dbPort == "" {
		dbPort = "3306"
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4", dbUser, dbPass, dbHost, dbPort, dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// ensureAccountsMigration ensures the accounts table exists and has token_validated_at column.
func ensureAccountsMigration() error {
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	if dbUser == "" || dbHost == "" || dbName == "" {
		return fmt.Errorf("missing DB env vars for migration")
	}
	if dbPort == "" {
		dbPort = "3306"
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4", dbUser, dbPass, dbHost, dbPort, dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Ensure accounts table exists with token_validated_at nullable
	acctCreate := `CREATE TABLE IF NOT EXISTS accounts (
		account_id VARCHAR(255) PRIMARY KEY,
		argon_token VARCHAR(512) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		token_validated_at TIMESTAMP NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, acctCreate); err != nil {
		return err
	}

	// If token_validated_at column doesn't exist, try to add it (safe with IF NOT EXISTS not supported on older MySQL)
	// We'll attempt an ALTER and ignore error if it's duplicate column.
	if _, err := db.ExecContext(ctx, "ALTER TABLE accounts ADD COLUMN token_validated_at TIMESTAMP NULL"); err != nil {
		// Check error text to see if column already exists; if so ignore.
		if !strings.Contains(err.Error(), "Duplicate column name") && !strings.Contains(err.Error(), "exists") {
			return err
		}
	}
	return nil
}
