package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type SaveRequest struct {
	AccountId  string `json:"accountId"`
	SaveData   string `json:"saveData"`
	LevelData  string `json:"levelData"`
	ArgonToken string `json:"argonToken"`
}

// UnmarshalJSON implements a tolerant JSON unmarshaller for SaveRequest.
// It accepts accountId as a number or string and recognizes both
// "accountId" and "accountID" keys.
func (s *SaveRequest) UnmarshalJSON(data []byte) error {
	// Decode into a generic map first
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Helper to fetch a string from possible key variations
	getStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil {
				switch t := v.(type) {
				case string:
					return t
				case float64:
					// JSON numbers decode as float64 — format without decimals
					return fmt.Sprintf("%.0f", t)
				case json.Number:
					return t.String()
				default:
					// fallback to string conversion
					return fmt.Sprintf("%v", t)
				}
			}
		}
		return ""
	}

	s.AccountId = getStr("accountId", "account_id")
	s.SaveData = getStr("saveData", "save_data")
	s.LevelData = getStr("levelData", "level_data")
	s.ArgonToken = getStr("argonToken", "argon_token")
	return nil
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

	// Read the full request body
	body, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		log.Printf("save: read body error: %v", readErr)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	// JSON unmarshal error handling with detailed logging
	if err := json.Unmarshal(body, &req); err != nil {
		bodyPreview := redactPreview(string(body), 200)
		log.Printf("save: json unmarshal error: %v content-type=%s bodyPreview=%s", err, r.Header.Get("Content-Type"), bodyPreview)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	// JSON unmarshal succeeded — log a redacted preview of saveData for diagnostics
	savePreview := redactPreview(req.SaveData, 120)
	levelPreview := redactPreview(req.LevelData, 120)
	argonPreview := redactPreview(req.ArgonToken, 80)
	log.Printf("save: parsed body as JSON (accountId='%s', saveDataPreview='%s', levelDataPreview='%s', argonTokenPreview='%s')", req.AccountId, savePreview, levelPreview, argonPreview)
	if req.AccountId == "" || req.SaveData == "" || req.LevelData == "" || req.ArgonToken == "" {
		// Log debugging info
		ct := r.Header.Get("Content-Type")
		bodyPreview := redactPreview(string(body), 200)
		log.Printf("save: missing accountId, saveData, levelData or argonToken (accountId='%s', saveDataPresent=%v, levelDataPresent=%v, argonTokenPresent=%v) content-type=%s bodyPreview=%s", req.AccountId, req.SaveData != "", req.LevelData != "", req.ArgonToken != "", ct, bodyPreview)
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
		level_data LONGTEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE KEY unique_account (account_id)
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

	// Ensure account row exists (insert if new)
	var storedToken sql.NullString
	row := db.QueryRowContext(ctx, "SELECT argon_token FROM accounts WHERE account_id = ?", req.AccountId)
	switch err := row.Scan(&storedToken); err {
	case sql.ErrNoRows:
		// First time account: create a row with provided token
		log.Printf("save: init POST for new account %s from %s", req.AccountId, r.RemoteAddr)
		if _, err := execWithRetries(ctx, db, "INSERT INTO accounts (account_id, argon_token) VALUES (?, ?)", req.AccountId, req.ArgonToken); err != nil {
			log.Printf("save: insert account error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
	case nil:
		// existing account: do nothing for now, validation happens below
	default:
		log.Printf("save: account lookup error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Validate token using Argon (or DB-stored token if ARGON_BASE_URL not configured)
	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Printf("save: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Printf("save: token validation failed for %s", req.AccountId)
		http.Error(w, "-1", http.StatusForbidden)
		return
	}

	// Overwrite existing save for this account if present, otherwise insert
	res, err := execWithRetries(ctx, db, "UPDATE saves SET save_data = ?, level_data = ?, created_at = CURRENT_TIMESTAMP WHERE account_id = ?", req.SaveData, req.LevelData, req.AccountId)
	if err != nil {
		// Detailed diagnostics to help identify whether this is a large-payload issue
		saveSize := len(req.SaveData)
		levelSize := len(req.LevelData)
		totalSize := saveSize + levelSize
		stats := db.Stats()
		log.Printf("save: update central save error: %v", err)
		log.Printf("save: payload sizes: save=%d bytes level=%d bytes total=%d bytes", saveSize, levelSize, totalSize)
		log.Printf("save: db stats: OpenConnections=%d InUse=%d Idle=%d WaitCount=%d MaxOpenConnections=%d", stats.OpenConnections, stats.InUse, stats.Idle, stats.WaitCount, stats.MaxOpenConnections)

		// Try a small probe write to check whether small writes succeed
		if _, perr := execWithRetries(ctx, db, "SET @probe = 1"); perr != nil {
			log.Printf("save: small probe write failed: %v", perr)
		} else {
			log.Printf("save: small probe write succeeded — large payload may be the cause")
		}

		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		if _, err := execWithRetries(ctx, db, "INSERT INTO saves (account_id, save_data, level_data) VALUES (?, ?, ?)", req.AccountId, req.SaveData, req.LevelData); err != nil {
			log.Printf("save: insert central save error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
	}

	// No per-account DBs or tables — all saves go into central `saves` table

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}

// redactPreview returns a shortened preview of s, masking long token-like patterns.
func redactPreview(s string, maxLen int) string {
	if s == "" {
		return "(empty)"
	}
	// Mask common token-like patterns: long dot-separated tokens
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	for i, p := range parts {
		if len(p) > 50 {
			// shorten and mask the middle
			if len(p) > 20 {
				parts[i] = p[:10] + "..." + p[len(p)-10:]
			} else {
				parts[i] = p[:10] + "..."
			}
		}
	}
	joined := strings.Join(parts, " ")
	if len(joined) > maxLen {
		return joined[:maxLen] + "..."
	}
	return joined
}

// isTransient tries to identify transient network/connection errors that may
// succeed on retry (e.g. connection reset by peer, i/o timeout, driver.BadConn).
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	// unwrap common wrapped errors
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection reset by peer"):
		return true
	case strings.Contains(msg, "broken pipe"):
		return true
	case strings.Contains(msg, "i/o timeout"):
		return true
	case strings.Contains(msg, "connection refused"):
		return true
	case strings.Contains(msg, "tls: handshake timeout"):
		return true
	case strings.Contains(msg, "use of closed network connection"):
		return true
	}
	return false
}

// execWithRetries executes a query and retries a few times when a transient
// error is detected. It respects the provided context for cancellation.
func execWithRetries(ctx context.Context, db *sql.DB, query string, args ...interface{}) (sql.Result, error) {
	var res sql.Result
	var err error
	backoff := 200 * time.Millisecond
	for attempt := 1; attempt <= 3; attempt++ {
		res, err = db.ExecContext(ctx, query, args...)
		if err == nil {
			return res, nil
		}
		if isTransient(err) && attempt < 3 {
			log.Printf("save: transient db error (attempt %d): %v; retrying after %s", attempt, err, backoff)
			select {
			case <-time.After(backoff):
				backoff *= 2
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		break
	}
	return res, err
}
