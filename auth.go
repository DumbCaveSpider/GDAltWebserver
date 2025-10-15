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
			log.Printf("auth: account not found: %s — creating account row", accountID)
			// create account row with provided token
			if _, cerr := db.ExecContext(ctx, "INSERT INTO accounts (account_id, argon_token) VALUES (?, ?)", accountID, token); cerr != nil {
				log.Printf("auth: failed to create account row for %s: %v", accountID, cerr)
				return false, cerr
			}
			return true, nil // consider valid after creation
		}
		// If the accounts table is missing, attempt to create it and retry once
		if strings.Contains(err.Error(), "1146") || strings.Contains(strings.ToLower(err.Error()), "doesn't exist") {
			log.Printf("auth: accounts table missing, attempting to create it: %v", err)
			if cerr := createAccountsTableIfMissing(ctx, db); cerr != nil {
				log.Printf("auth: failed to create accounts table: %v", cerr)
				return false, err
			}
			// retry the query once
			row = db.QueryRowContext(ctx, "SELECT argon_token, token_validated_at FROM accounts WHERE account_id = ?", accountID)
			if err2 := row.Scan(&storedToken, &tokenValidatedAt); err2 != nil {
				if err2 == sql.ErrNoRows {
					log.Printf("auth: account not found after creating accounts table: %s", accountID)
					return false, nil
				}
				log.Printf("auth: account lookup error after creating accounts table for %s: %v", accountID, err2)
				return false, err2
			}
		}
		log.Printf("auth: account lookup error for %s: %v", accountID, err)
		return false, err
	}

	if !storedToken.Valid || storedToken.String != token {
		log.Printf("auth: token mismatch for %s", accountID)
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
		log.Printf("auth: argon request error for %s: %v", accountID, err)
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Non-200 from Argon -> treat as transient error
		body, _ := io.ReadAll(resp.Body)
		log.Printf("auth: argon validation HTTP %d for %s: %s", resp.StatusCode, accountID, string(body))
		return false, fmt.Errorf("argon validation HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Parse response expecting JSON: { valid: true/false }
	var out struct {
		Valid bool `json:"valid"`
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("auth: error reading argon response for %s: %v", accountID, err)
		return false, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		log.Printf("auth: error parsing argon response JSON for %s: %v", accountID, err)
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
	log.Printf("auth: argon validation returned invalid for %s", accountID)
	return false, nil
}

func init() {
	http.HandleFunc("/auth", authHandler)
}

// createAccountsTableIfMissing creates the central accounts table if it does not exist.
func createAccountsTableIfMissing(ctx context.Context, db *sql.DB) error {
	create := `CREATE TABLE IF NOT EXISTS accounts (
		account_id VARCHAR(255) PRIMARY KEY,
		argon_token VARCHAR(512) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		token_validated_at TIMESTAMP NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, create); err != nil {
		return err
	}
	return nil
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
		log.Printf("auth: invalid method %s from %s", r.Method, r.RemoteAddr)
		http.Error(w, "-1", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("auth: read body error from %s: %v", r.RemoteAddr, err)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	var req authRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("auth: json unmarshal error from %s: %v (body len=%d)", r.RemoteAddr, err, len(body))
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Printf("auth: missing accountId or argonToken from %s (accountId='%s', tokenPresent=%v)", r.RemoteAddr, req.AccountId, req.ArgonToken != "")
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
		log.Printf("auth: db open error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Printf("auth: db ping error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Printf("auth: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Printf("auth: token invalid for %s", req.AccountId)
		http.Error(w, "-1", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}
