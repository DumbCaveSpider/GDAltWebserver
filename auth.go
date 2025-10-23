package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"
)

// ValidateArgonToken validates the provided argon token for accountID by calling
// Argon's validation endpoint. If valid, it creates/updates the account row with the token.
//
// Returns (true, nil) when token is valid, (false, nil) when invalid, and (false, err)
// when a transient error occurred.
func ValidateArgonToken(ctx context.Context, db *sql.DB, accountID, token string) (bool, error) {
	// Always call Argon's validation endpoint to verify token
	base := "https://argon.globed.dev/v1/validation/check"
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("account_id", accountID)
	q.Set("authtoken", token)
	u.RawQuery = q.Encode()
	argonURL := u.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, argonURL, nil)
	if err != nil {
		log.Warn("auth: failed to create argon request for %s: %v", accountID, err)
		return false, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("auth: argon request error for %s: %v", accountID, err)
		return false, err
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Warn("auth: error reading argon response for %s: %v", accountID, err)
		return false, err
	}

	if resp.StatusCode != http.StatusOK {
		log.Warn("auth: argon validation HTTP %d for %s: %s", resp.StatusCode, accountID, string(body))
		return false, nil // invalid token
	}

	// Parse response expecting JSON: { valid: true/false }
	var out struct {
		Valid bool `json:"valid"`
	}

	// Log the raw response for debugging
	log.Debug("auth: argon response for %s (status %d): %s", accountID, resp.StatusCode, string(body))

	if err := json.Unmarshal(body, &out); err != nil {
		log.Warn("auth: error parsing argon response JSON for %s: %v", accountID, err)
		return false, err
	}

	if !out.Valid {
		log.Warn("auth: argon validation returned valid=false for %s", accountID)
		return false, nil
	}

	// Token is valid - ensure account row exists and update token
	log.Info("auth: argon validation successful for %s", accountID)

	// Check if account exists
	var existingToken sql.NullString
	row := db.QueryRowContext(ctx, "SELECT argon_token FROM accounts WHERE account_id = ?", accountID)
	err = row.Scan(&existingToken)

	if err == sql.ErrNoRows {
		// Account doesn't exist - create it
		log.Info("auth: creating new account row for %s", accountID)
		if _, cerr := db.ExecContext(ctx, "INSERT INTO accounts (account_id, argon_token, token_validated_at) VALUES (?, ?, CURRENT_TIMESTAMP)", accountID, token); cerr != nil {
			log.Error("auth: failed to create account row for %s: %v", accountID, cerr)
			return false, cerr
		}
	} else if err != nil {
		// Database error
		log.Error("auth: account lookup error for %s: %v", accountID, err)
		return false, err
	} else {
		// Account exists - update token and timestamp
		log.Info("auth: updating token for existing account %s", accountID)
		if _, uerr := db.ExecContext(ctx, "UPDATE accounts SET argon_token = ?, token_validated_at = CURRENT_TIMESTAMP WHERE account_id = ?", token, accountID); uerr != nil {
			log.Error("auth: failed to update token for %s: %v", accountID, uerr)
			return false, uerr
		}
	}

	return true, nil
}

func init() {
	http.HandleFunc("/auth", authHandler)
}

type authRequest struct {
	AccountId  string `json:"accountId"`
	ArgonToken string `json:"argonToken"`
}

// UnmarshalJSON accepts accountId as a number or string.
func (a *authRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// helper
	get := func(keys ...string) string {
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
	a.AccountId = get("accountId", "account_id")
	a.ArgonToken = get("argonToken", "argon_token")
	return nil
}

// authHandler accepts POST JSON { accountId, argonToken } and returns plain text
// "1" when the token is valid for the account, or "-1" on failure.
func authHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Warn("auth: invalid method %s from %s", r.Method, r.RemoteAddr)
		http.Error(w, "-1", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("auth: read body error from %s: %v", r.RemoteAddr, err)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	var req authRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Warn("auth: json unmarshal error from %s: %v (body len=%d)", r.RemoteAddr, err, len(body))
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("auth: missing accountId or argonToken from %s (accountId='%s', tokenPresent=%v)", r.RemoteAddr, req.AccountId, req.ArgonToken != "")
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
		log.Error("auth: db open error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Error("auth: db ping error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("auth: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Warn("auth: token invalid for %s", req.AccountId)
		http.Error(w, "-1", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}
