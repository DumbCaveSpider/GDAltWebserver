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

type LoadRequest struct {
	AccountId  string `json:"accountId"`
	ArgonToken string `json:"argonToken"`
}

func (l *LoadRequest) UnmarshalJSON(data []byte) error {
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
				default:
					return fmt.Sprintf("%v", t)
				}
			}
		}
		return ""
	}
	l.AccountId = get("accountId", "account_id")
	l.ArgonToken = get("argonToken", "argon_token")
	return nil
}

func init() {
	http.HandleFunc("/load", loadHandler)
}

func loadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		log.Debug("load: invalid method %s", r.Method)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("load: read body error: %v", err)
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}

	var req LoadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Warn("load: json unmarshal error: %v", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("load: missing accountId or argonToken")
		http.Error(w, "Missing Account ID or Argon Token", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	db := DB
	if db == nil {
		log.Error("load: DB not initialized")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("load: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Warn("load: token validation failed for %s", req.AccountId)
		http.Error(w, "Token validation failed", http.StatusForbidden)
		return
	}

	var saveData sql.NullString
	r2 := db.QueryRowContext(ctx, "SELECT save_data FROM saves WHERE account_id = ?", req.AccountId)
	if err := r2.Scan(&saveData); err != nil {
		if err == sql.ErrNoRows {
			// no save found
			http.Error(w, "Save data not found", http.StatusNotFound)
			return
		}
		log.Error("load: save lookup error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if !saveData.Valid {
		http.Error(w, "Save data invalid", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(saveData.String))
}
