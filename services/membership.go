package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"
)

type MembershipRequest struct {
	Email      string `json:"email"`
	AccountId  string `json:"accountId"`
	ArgonToken string `json:"argonToken"`
}

func (m *MembershipRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	getStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil {
				switch t := v.(type) {
				case string:
					return t
				case float64:
					return fmt.Sprintf("%.0f", t)
				case json.Number:
					return t.String()
				default:
					return fmt.Sprintf("%v", t)
				}
			}
		}
		return ""
	}

	m.Email = getStr("email")
	m.AccountId = getStr("accountId", "account_id")
	m.ArgonToken = getStr("argonToken", "argon_token")
	return nil
}

func init() {
	http.HandleFunc("/membership", membershipHandler)
}

func ensureMembershipsTable(ctx context.Context, db *sql.DB) error {

	// Create table if not exists (matching schema + account_id + expires_at)
	query := `CREATE TABLE IF NOT EXISTS memberships (
		id INT AUTO_INCREMENT PRIMARY KEY,
		kofi_transaction_id VARCHAR(255),
		email VARCHAR(255),
		discord_username VARCHAR(255),
		discord_userid VARCHAR(255),
		tier_name VARCHAR(255),
		account_id VARCHAR(255),
		expires_at TIMESTAMP NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, query); err != nil {
		return err
	}

	// Ensure account_id column exists (migration for existing table)
	if _, err := db.ExecContext(ctx, "ALTER TABLE memberships ADD COLUMN account_id VARCHAR(255)"); err != nil {
		if !strings.Contains(err.Error(), "Duplicate column name") && !strings.Contains(err.Error(), "exists") {
			return err
		}
	}

	// Ensure expires_at column exists
	if _, err := db.ExecContext(ctx, "ALTER TABLE memberships ADD COLUMN expires_at TIMESTAMP NULL"); err != nil {
		if !strings.Contains(err.Error(), "Duplicate column name") && !strings.Contains(err.Error(), "exists") {
			return err
		}
	}
	return nil
}

func membershipHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Method not allowed"})
		return
	}

	body, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		log.Warn("membership: read body error: %v", readErr)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Failed to read request"})
		return
	}

	var req MembershipRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Warn("membership: json unmarshal error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Invalid request"})
		return
	}

	if req.AccountId == "" || req.ArgonToken == "" || req.Email == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Missing required field"})
		return
	}

	// DB Connection
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	if dbPort == "" {
		dbPort = "3306"
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4", dbUser, dbPass, dbHost, dbPort, dbName)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Error("membership: db open error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}
	defer db.Close()

	if err := ensureMembershipsTable(ctx, db); err != nil {
		log.Error("membership: table migration error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	// Validate Argon
	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("membership: token validation error for %s: %v", req.AccountId, verr)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}
	if !ok {
		log.Warn("membership: token invalid for %s", req.AccountId)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Invalid Argon Token"})
		return
	}

	var count int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memberships WHERE email = ?", req.Email).Scan(&count)
	if err != nil {
		log.Error("membership: email lookup error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	if count == 0 {
		// Email not found in memberships
		log.Info("membership: email %s not found for account %s", req.Email, req.AccountId)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Email not found in memberships"})
		return
	}

	// Email found
	log.Info("membership: found %d matches for email %s (account %s)", count, req.Email, req.AccountId)

	// Transaction to update both tables
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		log.Error("membership: tx begin error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}
	defer tx.Rollback()

	// 1. Link email to accountId in memberships table
	if _, err := tx.ExecContext(ctx, "UPDATE memberships SET account_id = ? WHERE email = ?", req.AccountId, req.Email); err != nil {
		log.Error("membership: failed to link memberships: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	// 2. Check if any now-linked membership is valid (unexpired)
	var validCount int
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memberships WHERE account_id = ? AND (expires_at > NOW() OR expires_at IS NULL)", req.AccountId).Scan(&validCount)
	if err != nil {
		log.Error("membership: failed to check validity: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	if validCount > 0 {
		// Grant subscriber status
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET subscriber = 1 WHERE account_id = ?", req.AccountId); err != nil {
			log.Error("membership: failed to update account subscriber status: %v", err)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
			return
		}
		log.Info("membership: granted subscriber status to %s", req.AccountId)
	} else {
		log.Info("membership: linked memberships for %s but none are active/unexpired", req.AccountId)
	}

	if err := tx.Commit(); err != nil {
		log.Error("membership: tx commit error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	log.Info("membership: successfully applied membership for %s (email: %s)", req.AccountId, req.Email)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("1"))
}
