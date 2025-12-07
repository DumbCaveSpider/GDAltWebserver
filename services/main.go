package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"
	_ "github.com/go-sql-driver/mysql"
)

func main() {
	if err := checkDB(); err != nil {
		log.Error("DB check failed: %v", err)
	} else {
		log.Done("DB check: connected OK")
	}

	if err := ensureAccountsMigration(); err != nil {
		log.Warn("DB migration warning: %v", err)
	}

	// Ensure saves table exists as well
	if err := ensureSavesMigration(); err != nil {
		log.Warn("DB migration warning (saves): %v", err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Print("pong: %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("🗣️🔥"))
	})

	startCleanupRoutine()

	addr := ":3001"
	log.Done("starting server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Error("server failed: %v", err)
	}
}

func openDBConnection() (*sql.DB, error) {
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	if dbUser == "" || dbHost == "" || dbName == "" {
		return nil, fmt.Errorf("missing DB env vars (DB_USER, DB_HOST, DB_NAME required)")
	}
	if dbPort == "" {
		dbPort = "3306"
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4", dbUser, dbPass, dbHost, dbPort, dbName)
	return sql.Open("mysql", dsn)
}

func startCleanupRoutine() {
	go runCleanup()

	// cleanup every 24 hours
	go func() {
		log.Info("cleanup: scheduler started (interval: 24h)")
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			runCleanup()
		}
	}()
}

func runCleanup() {
	log.Debug("cleanup: checking for inactive accounts...")
	db, err := openDBConnection()
	if err != nil {
		log.Error("cleanup: failed to connect to db: %v", err)
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	query := `DELETE a, s 
              FROM accounts a 
              JOIN saves s ON a.account_id = s.account_id 
              WHERE s.created_at < DATE_SUB(NOW(), INTERVAL 100 DAY)`

	res, err := db.ExecContext(ctx, query)
	if err != nil {
		log.Error("cleanup: failed to delete inactive accounts: %v", err)
		return
	}

	if rows, err := res.RowsAffected(); err == nil && rows > 0 {
		log.Info("cleanup: removed %d inactive rows (accounts+saves)", rows)
	} else {
		log.Debug("cleanup: no inactive accounts found")
	}
}

func checkDB() error {
	db, err := openDBConnection()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

func ensureAccountsMigration() error {
	db, err := openDBConnection()
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	acctCreate := `CREATE TABLE IF NOT EXISTS accounts (
		account_id VARCHAR(255) PRIMARY KEY,
		argon_token VARCHAR(512) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		token_validated_at TIMESTAMP NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, acctCreate); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, "ALTER TABLE accounts ADD COLUMN token_validated_at TIMESTAMP NULL"); err != nil {
		if !strings.Contains(err.Error(), "Duplicate column name") && !strings.Contains(err.Error(), "exists") {
			return err
		}
	}
	return nil
}

func ensureSavesMigration() error {
	db, err := openDBConnection()
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createStmt := `CREATE TABLE IF NOT EXISTS saves (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		account_id VARCHAR(255) NOT NULL,
		save_data LONGTEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE KEY unique_account (account_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`

	if _, err := db.ExecContext(ctx, createStmt); err != nil {
		return err
	}
	return nil
}
