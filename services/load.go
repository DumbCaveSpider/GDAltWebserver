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
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Method not allowed"})
		log.Debug("load: invalid method %s", r.Method)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("load: read body error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Failed to read request"})
		return
	}

	var req LoadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Warn("load: json unmarshal error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Invalid request"})
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("load: missing accountId or argonToken")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Missing Account ID or Argon Token"})
		return
	}

	// Build DSN
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
		log.Error("load: db open error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Error("load: db ping error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("load: token validation error for %s: %v", req.AccountId, verr)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}
	if !ok {
		log.Warn("load: token validation failed for %s", req.AccountId)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Token validation failed"})
		return
	}

	var saveData sql.NullString
	r2 := db.QueryRowContext(ctx, "SELECT save_data FROM saves WHERE account_id = ?", req.AccountId)
	if err := r2.Scan(&saveData); err != nil {
		if err == sql.ErrNoRows {
			// no save found
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "Save data not found"})
			return
		}
		log.Error("load: save lookup error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	if !saveData.Valid {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Save data invalid"})
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(saveData.String))
}
