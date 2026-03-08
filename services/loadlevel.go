package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"time"

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"
)

func init() {
	http.HandleFunc("/loadlevel", loadLevelHandler)
}

func loadLevelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		log.Debug("loadlevel: invalid method %s", r.Method)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("loadlevel: read body error: %v", err)
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}

	var req LoadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Error("loadlevel: json unmarshal error: %v", err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("loadlevel: missing accountId or argonToken")
		http.Error(w, "Missing Account ID or Argon Token", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	db := DB
	if db == nil {
		log.Error("loadlevel: DB not initialized")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("loadlevel: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Warn("loadlevel: token validation failed for %s", req.AccountId)
		http.Error(w, "Invalid Argon Token", http.StatusForbidden)
		return
	}

	var levelData sql.NullString
	r2 := db.QueryRowContext(ctx, "SELECT level_data FROM saves WHERE account_id = ?", req.AccountId)
	if err := r2.Scan(&levelData); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Level data not found", http.StatusNotFound)
			return
		}
		log.Error("loadlevel: save lookup error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if !levelData.Valid {
		http.Error(w, "Level data invalid", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(levelData.String))
}
