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

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"
	_ "github.com/go-sql-driver/mysql"
)

type PaymentRequest struct {
	MessageID                  string      `json:"message_id"`
	Timestamp                  string      `json:"timestamp"`
	Type                       string      `json:"type"`
	IsPublic                   bool        `json:"is_public"`
	FromName                   string      `json:"from_name"`
	Message                    interface{} `json:"message"` // null or string
	Amount                     string      `json:"amount"`
	URL                        string      `json:"url"`
	Email                      string      `json:"email"`
	Currency                   string      `json:"currency"`
	IsSubscriptionPayment      bool        `json:"is_subscription_payment"`
	IsFirstSubscriptionPayment bool        `json:"is_first_subscription_payment"`
	KofiTransactionID          string      `json:"kofi_transaction_id"`
	ShopItems                  interface{} `json:"shop_items"`
	TierName                   string      `json:"tier_name"`
	Shipping                   interface{} `json:"shipping"`
	DiscordUsername            string      `json:"discord_username"`
	DiscordUserID              string      `json:"discord_userid"`
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

	// Required fields check (basic)
	if req.KofiTransactionID == "" {
		log.Warn("payment: missing kofi_transaction_id")
	}

	log.Info("payment: received transaction %s type=%s tier='%s' user='%s'", req.KofiTransactionID, req.Type, req.TierName, req.DiscordUsername)

	if req.TierName == "Account Backup Extra" {
		if err := processMembership(r.Context(), req); err != nil {
			log.Error("payment: failed to process membership: %v", err)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
			return
		}
		log.Done("payment: processed membership for %s (tier: %s)", req.DiscordUsername, req.TierName)
	} else {
		log.Info("payment: ignoring tier '%s'", req.TierName)
	}

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

	insertStmt := `INSERT INTO memberships (kofi_transaction_id, email, discord_username, discord_userid, tier_name) VALUES (?, ?, ?, ?, ?)`

	// Use explicit args
	_, err = db.ExecContext(ctx, insertStmt, req.KofiTransactionID, req.Email, req.DiscordUsername, req.DiscordUserID, req.TierName)
	if err != nil {
		return fmt.Errorf("insert error: %v", err)
	}

	return nil
}
