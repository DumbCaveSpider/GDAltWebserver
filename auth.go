package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ValidateArgonToken validates the provided argon token for accountID.
// It first checks the stored token in DB matches the provided token. If so,
// it may optionally call Argon's remote validation endpoint to verify token
// freshness and caches the result by updating accounts.token_validated_at.
//
// Returns (true, nil) when token is valid, (false, nil) when invalid, and (false, err)
// when a transient error occurred.
func ValidateArgonToken(ctx context.Context, db *sql.DB, accountID, token string) (bool, error) {
	// Quick DB lookup to ensure account exists and token matches
	var storedToken sql.NullString
	var tokenValidatedAt sql.NullTime
	row := db.QueryRowContext(ctx, "SELECT argon_token, token_validated_at FROM accounts WHERE account_id = ?", accountID)
	if err := row.Scan(&storedToken, &tokenValidatedAt); err != nil {
		if err == sql.ErrNoRows {
			return false, nil // account not found
		}
		return false, err
	}

	if !storedToken.Valid || storedToken.String != token {
		return false, nil // token mismatch
	}

	// If token_validated_at is recent (within TTL) consider it valid without contacting Argon.
	ttl := 5 * time.Minute
	if tv := tokenValidatedAt; tv.Valid {
		if time.Since(tv.Time) <= ttl {
			return true, nil
		}
	}

	// Call Argon's validation endpoint if configured
	argonBase := os.Getenv("ARGON_BASE_URL")
	if argonBase == "" {
		// Not configured: accept DB-stored token as authoritative
		return true, nil
	}

	// Build request: GET {ARGON_BASE_URL}/v1/validation/check?token={token}
	url := strings.TrimRight(argonBase, "/") + "/v1/validation/check?token=" + token
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}

	// Optional: allow ARGON_API_KEY to be used as a header when present
	if k := os.Getenv("ARGON_API_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Non-200 from Argon -> treat as transient error
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("argon validation HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Parse response expecting JSON: { valid: true/false }
	var out struct {
		Valid bool `json:"valid"`
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return false, err
	}

	if out.Valid {
		// Update token_validated_at to now
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET token_validated_at = CURRENT_TIMESTAMP WHERE account_id = ?", accountID); err != nil {
			// Log but still return success
			log.Printf("auth: failed to update token_validated_at for %s: %v", accountID, err)
		}
		return true, nil
	}
	return false, nil
}

func init() {
	http.HandleFunc("/auth", authHandler)
}

type authRequest struct {
	AccountId  string `json:"accountId"`
	ArgonToken string `json:"argonToken"`
}

// authHandler accepts POST JSON { accountId, argonToken } and returns plain text
// "1" when the token is valid for the account, or "-1" on failure.
func authHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "-1", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	var req authRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	// Build DSN from environment variables (same as other handlers)
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	if dbPort == "" {
		dbPort = "3306"
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4", dbUser, dbPass, dbHost, dbPort, dbName)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "-1", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}
