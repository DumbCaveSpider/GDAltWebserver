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

func ValidateArgonToken(ctx context.Context, db *sql.DB, accountID, token string) (bool, error) {
	// Check cache
	var cachedToken sql.NullString
	var validatedAt sql.NullTime
	err := db.QueryRowContext(ctx, "SELECT argon_token, token_validated_at FROM accounts WHERE account_id = ?", accountID).Scan(&cachedToken, &validatedAt)
	if err == nil && cachedToken.Valid && cachedToken.String == token && validatedAt.Valid {
		// Cache valid for 15 minutes
		if time.Since(validatedAt.Time) < 15*time.Minute {
			log.Info("auth: using cached validation for %s", accountID)
			return true, nil
		}
	}

	base := os.Getenv("ARGON_BASE_URL")
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("account_id", accountID)
	q.Set("authtoken", token)
	u.RawQuery = q.Encode()
	argonURL := u.String()

	var resp *http.Response
	var reqErr error

	// Retry logic for 429
	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, argonURL, nil)
		if err != nil {
			log.Warn("auth: failed to create argon request for %s: %v", accountID, err)
			return false, err
		}

		client := &http.Client{Timeout: 10 * time.Second}
		resp, reqErr = client.Do(req)
		if reqErr != nil {
			log.Warn("auth: argon request error for %s: %v", accountID, reqErr)
			return false, reqErr
		}

		if resp.StatusCode == 429 {
			if i < 2 {
				resp.Body.Close()
				log.Warn("auth: argon rate limit checking %s (attempt %d/3), waiting...", accountID, i+1)
				select {
				case <-ctx.Done():
					return false, ctx.Err()
				case <-time.After(time.Duration(1<<i) * time.Second):
					continue
				}
			}
		}
		break
	}

	if resp == nil {
		if reqErr != nil {
			return false, reqErr
		}
		return false, fmt.Errorf("unknown error: no response")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Warn("auth: error reading argon response for %s: %v", accountID, err)
		return false, err
	}

	if resp.StatusCode != http.StatusOK {
		log.Warn("auth: argon validation HTTP %d for %s: %s", resp.StatusCode, accountID, string(body))
		if resp.StatusCode == 429 {
			return false, fmt.Errorf("rate limit exceeded")
		}
		return false, nil // invalid token
	}

	// Parse response expecting JSON: { valid: true/false }
	var out struct {
		Valid bool `json:"valid"`
	}

	log.Debug("auth: argon response for %s (status %d): %s", accountID, resp.StatusCode, string(body))

	if err := json.Unmarshal(body, &out); err != nil {
		log.Warn("auth: error parsing argon response JSON for %s: %v", accountID, err)
		return false, err
	}

	if !out.Valid {
		log.Warn("auth: argon validation returned valid=false for %s", accountID)
		return false, nil
	}

	log.Info("auth: argon validation successful for %s", accountID)

	var existingToken sql.NullString
	// Re-check DB for update
	row := db.QueryRowContext(ctx, "SELECT argon_token FROM accounts WHERE account_id = ?", accountID)
	err = row.Scan(&existingToken)

	if err == sql.ErrNoRows {
		log.Info("auth: creating new account row for %s", accountID)
		if _, cerr := db.ExecContext(ctx, "INSERT INTO accounts (account_id, argon_token, token_validated_at) VALUES (?, ?, CURRENT_TIMESTAMP)", accountID, token); cerr != nil {
			log.Error("auth: failed to create account row for %s: %v", accountID, cerr)
			return false, cerr
		}
	} else if err != nil {
		log.Error("auth: account lookup error for %s: %v", accountID, err)
		return false, err
	} else {
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

func authHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Debug("auth: invalid method %s from %s", r.Method, r.RemoteAddr)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("auth: read body error from %s: %v", r.RemoteAddr, err)
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}

	var req authRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Warn("auth: json unmarshal error from %s: %v (body len=%d)", r.RemoteAddr, err, len(body))
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("auth: missing accountId or argonToken (accountId='%s', tokenPresent=%v)", req.AccountId, req.ArgonToken != "")
		http.Error(w, "Missing Account ID or Argon Token", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	db := DB
	if db == nil {
		log.Error("auth: DB not initialized")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("auth: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Warn("auth: token invalid for %s", req.AccountId)
		http.Error(w, "Invalid Argon Token", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}
