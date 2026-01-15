// If you are self-hosting, don't use this endpoint as this is mainly used for the main server.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"
	_ "github.com/go-sql-driver/mysql"
)

type PaymentRequest struct {
	Type              string `json:"type"`
	VerificationToken string `json:"verificationToken"`
	Email             string `json:"email"`
	DiscordUsername   string `json:"discord_username"`
	DiscordUserID     string `json:"discord_userid"`
	KofiTransactionID string `json:"kofi_transaction_id"`
}

func init() {
	http.HandleFunc("/payment", paymentHandler)
}

func paymentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Method not allowed"})
		log.Warn("payment: invalid method %s", r.Method)
		return
	}

	body, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		log.Warn("payment: read body error: %v", readErr)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Failed to read request"})
		return
	}

	var req PaymentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Warn("payment: json unmarshal error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Invalid request"})
		return
	}

	// Validate Verification Token
	envToken := os.Getenv("VERIFICATION_TOKEN")
	if envToken == "" {
		log.Warn("payment: missing verification token")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Missing verification token"})
		return
	}

	if req.VerificationToken != envToken {
		log.Warn("payment: invalid verification token for %s", req.Email)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Invalid verification token"})
		return
	}

	if req.KofiTransactionID == "" {
		log.Warn("payment: missing kofi_transaction_id")
	}

	log.Info("payment: received transaction %s type=%s user='%s'", req.KofiTransactionID, req.Type, req.DiscordUsername)

	if err := processMembership(r.Context(), req); err != nil {
		log.Error("payment: failed to process membership: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}
	log.Done("payment: processed membership for %s", req.DiscordUsername)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func processMembership(ctx context.Context, req PaymentRequest) error {
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")

	if dbPort == "" {
		dbPort = "3306"
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4", dbUser, dbPass, dbHost, dbPort, dbName)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("db open error: %v", err)
	}
	defer db.Close()

	var existingID int64
	var currentExpires sql.NullTime
	var existingAccountID sql.NullString

	err = db.QueryRowContext(ctx, "SELECT id, expires_at, account_id FROM memberships WHERE email = ? ORDER BY id DESC LIMIT 1", req.Email).Scan(&existingID, &currentExpires, &existingAccountID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("lookup error: %v", err)
	}

	if err == nil {
		start := time.Now()
		if currentExpires.Valid && currentExpires.Time.After(start) {
			start = currentExpires.Time
		}
		newExpiry := start.AddDate(0, 1, 0) // Add 1 month

		log.Info("payment: updating membership %d for %s (new expiry: %v)", existingID, req.Email, newExpiry)
		_, err = db.ExecContext(ctx, "UPDATE memberships SET expires_at = ?, kofi_transaction_id = ? WHERE id = ?", newExpiry, req.KofiTransactionID, existingID)
		if err != nil {
			return fmt.Errorf("update error: %v", err)
		}

		// Update subscriber status if account is linked
		if existingAccountID.Valid && existingAccountID.String != "" {
			if _, err := db.ExecContext(ctx, "UPDATE accounts SET subscriber = 1 WHERE account_id = ?", existingAccountID.String); err != nil {
				log.Warn("payment: failed to re-enable subscriber status for %s: %v", existingAccountID.String, err)
			}
		}

	} else {
		newExpiry := time.Now().AddDate(0, 1, 0)
		log.Info("payment: creating new membership for %s (expiry: %v)", req.Email, newExpiry)
		insertStmt := `INSERT INTO memberships (kofi_transaction_id, email, discord_username, discord_userid, tier_name, expires_at) VALUES (?, ?, ?, ?, ?, ?)`

		_, err = db.ExecContext(ctx, insertStmt, req.KofiTransactionID, req.Email, req.DiscordUsername, req.DiscordUserID, "Account Backup Extra", newExpiry)
		if err != nil {
			return fmt.Errorf("insert error: %v", err)
		}
	}

	return nil
}
