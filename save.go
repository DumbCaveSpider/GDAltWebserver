package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type SaveRequest struct {
	AccountId  string `json:"accountId"`
	SaveData   string `json:"saveData"`
	ArgonToken string `json:"argonToken"`
}

func init() {
	// register handler on the default mux used by main.go
	http.HandleFunc("/save", saveHandler)
}

func saveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "-1", http.StatusMethodNotAllowed)
		log.Printf("save: invalid method %s", r.Method)
		return
	}

	var req SaveRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {

		if err := r.ParseForm(); err == nil && len(r.Form) > 0 {
			// Accept form fields: accountId, accountID, saveData, argonToken
			if v := r.Form.Get("accountId"); v != "" {
				req.AccountId = v
			} else if v := r.Form.Get("accountID"); v != "" {
				req.AccountId = v
			}
			req.SaveData = r.Form.Get("saveData")
			req.ArgonToken = r.Form.Get("argonToken")
		} else {
			log.Printf("save: primary JSON decode error: %v", err)
			// Return bad request — recommend clients send accountId as a string
			http.Error(w, "-1", http.StatusBadRequest)
			return
		}
	}
	if req.AccountId == "" || req.SaveData == "" || req.ArgonToken == "" {
		log.Printf("save: missing accountId, saveData or argonToken")
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	// Build DSN from environment variables
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	if dbUser == "" || dbHost == "" || dbName == "" {
		log.Printf("save: missing DB_* env vars (DB_USER, DB_HOST, DB_NAME required)")
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if dbPort == "" {
		dbPort = "3306"
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4", dbUser, dbPass, dbHost, dbPort, dbName)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Printf("save: db open error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Printf("save: db ping error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Ensure central saves table exists
	createStmt := `CREATE TABLE IF NOT EXISTS saves (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		account_id VARCHAR(255) NOT NULL,
		save_data LONGTEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, createStmt); err != nil {
		log.Printf("save: create table error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Ensure accounts table exists in central DB
	acctCreate := `CREATE TABLE IF NOT EXISTS accounts (
		account_id VARCHAR(255) PRIMARY KEY,
		argon_token VARCHAR(512) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, acctCreate); err != nil {
		log.Printf("save: create accounts table error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Helper to sanitize database name (allow alnum and underscore, otherwise sha1)
	sanitize := func(s string) string {
		re := regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
		if re.MatchString(s) {
			return s
		}
		h := sha1.Sum([]byte(s))
		return hex.EncodeToString(h[:])
	}
	accDbName := "acct_" + sanitize(req.AccountId)

	// Check if account exists in accounts table
	var storedToken sql.NullString
	row := db.QueryRowContext(ctx, "SELECT argon_token FROM accounts WHERE account_id = ?", req.AccountId)
	switch err := row.Scan(&storedToken); err {
	case sql.ErrNoRows:
		// First time account: log an init POST (don't log secrets)
		log.Printf("save: init POST for new account %s from %s", req.AccountId, r.RemoteAddr)
		// First time account: insert into accounts and attempt to create per-account database
		if _, err := db.ExecContext(ctx, "INSERT INTO accounts (account_id, argon_token) VALUES (?, ?)", req.AccountId, req.ArgonToken); err != nil {
			log.Printf("save: insert account error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
		// Create per-account database
		createDbStmt := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;", accDbName)
		if _, err := db.ExecContext(ctx, createDbStmt); err != nil {
			log.Printf("save: create account database error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
		// Create saves table inside per-account DB
		createPerAcct := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s`.saves ( id BIGINT AUTO_INCREMENT PRIMARY KEY, account_id VARCHAR(255) NOT NULL, save_data LONGTEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;", accDbName)
		if _, err := db.ExecContext(ctx, createPerAcct); err != nil {
			log.Printf("save: create per-account saves table error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
	case nil:
		// existing account: verify token matches
		if !storedToken.Valid || storedToken.String != req.ArgonToken {
			log.Printf("save: argon token mismatch for account %s", req.AccountId)
			http.Error(w, "-1", http.StatusForbidden)
			return
		}
	default:
		log.Printf("save: account lookup error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Insert the save into central saves table
	if _, err := db.ExecContext(ctx, "INSERT INTO saves (account_id, save_data) VALUES (?, ?)", req.AccountId, req.SaveData); err != nil {
		log.Printf("save: insert central save error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Also insert into per-account DB saves table (if exists)
	perAcctInsert := fmt.Sprintf("INSERT INTO `%s`.saves (account_id, save_data) VALUES (?, ?)", accDbName)
	if _, err := db.ExecContext(ctx, perAcctInsert, req.AccountId, req.SaveData); err != nil {
		log.Printf("save: insert per-account save error (continuing): %v", err)
		// don't fail the whole request for per-account insert failure — we already inserted centrally
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}
