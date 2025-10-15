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
