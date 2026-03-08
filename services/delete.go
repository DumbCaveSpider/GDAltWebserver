package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"
)

type DeleteRequest struct {
	AccountId  string `json:"accountId"`
	ArgonToken string `json:"argonToken"`
}

func (d *DeleteRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil {
				switch t := v.(type) {
				case string:
					return t
				case float64:
					return fmt.Sprintf("%.0f", t)
				default:
					return fmt.Sprintf("%v", t)
				}
			}
		}
		return ""
	}
	d.AccountId = get("accountId", "account_id")
	d.ArgonToken = get("argonToken", "argon_token")
	return nil
}

func init() {
	http.HandleFunc("/delete", deleteHandler)
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		log.Debug("delete: invalid method %s", r.Method)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("delete: read body error: %v", err)
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}
	var req DeleteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Warn("delete: json unmarshal error: %v", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("delete: missing accountId or argonToken")
		http.Error(w, "Missing Account ID or Argon Token", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	db := DB
	if db == nil {
		log.Error("delete: DB not initialized")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var storedToken sql.NullString
	row := db.QueryRowContext(ctx, "SELECT argon_token FROM accounts WHERE account_id = ?", req.AccountId)
	switch err := row.Scan(&storedToken); err {
	case sql.ErrNoRows:
		log.Warn("delete: account not found %s", req.AccountId)
		http.Error(w, "Account not found", http.StatusForbidden)
		return
	case nil:
		if !storedToken.Valid || storedToken.String != req.ArgonToken {
			log.Warn("delete: argon token mismatch for account %s", req.AccountId)
			http.Error(w, "Invalid argon token", http.StatusForbidden)
			return
		}
	default:
		log.Error("delete: account lookup error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM saves WHERE account_id = ?", req.AccountId); err != nil {
		log.Error("delete: delete save error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}
