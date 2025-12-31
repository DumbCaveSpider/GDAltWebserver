package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"
)

type CheckRequest struct {
	AccountId  string `json:"accountId"`
	ArgonToken string `json:"argonToken"`
}

func (c *CheckRequest) UnmarshalJSON(data []byte) error {
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
	c.AccountId = get("accountId", "account_id")
	c.ArgonToken = get("argonToken", "argon_token")
	return nil
}

func init() {
	http.HandleFunc("/check", checkHandler)
}

func checkHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Method not allowed"})
		log.Debug("check: invalid method %s", r.Method)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("check: read body error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Failed to read request"})
		return
	}
	var req CheckRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Warn("check: json unmarshal error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Invalid request"})
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("check: missing accountId or argonToken")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Missing Account ID or Argon Token"})
		return
	}

	maxDataSize := 33554432
	if v := os.Getenv("MAX_DATA_SIZE_BYTES"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			maxDataSize = parsed
		}
	}

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
		log.Error("check: db open error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		log.Error("check: db ping error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	var storedToken sql.NullString
	var isSubscriber bool
	// Note: subscriber column usage
	row := db.QueryRowContext(ctx, "SELECT argon_token, subscriber FROM accounts WHERE account_id = ?", req.AccountId)
	switch err := row.Scan(&storedToken, &isSubscriber); err {
	case sql.ErrNoRows:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Account not found"})
		return
	case nil:
		if !storedToken.Valid || storedToken.String != req.ArgonToken {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "Invalid Argon Token"})
			return
		}
	default:
		log.Error("check: account lookup error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}

	// Adjust maxDataSize for subscribers
	if isSubscriber {
		// Default 128MB
		maxDataSize = 134217728
		// Override from env if present
		if v := os.Getenv("SUBSCRIBER_MAX_DATA_SIZE_BYTES"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
				maxDataSize = parsed
			}
		}
	}

	var saveData, levelData sql.NullString
	var createdAt sql.NullTime
	r2 := db.QueryRowContext(ctx, "SELECT save_data, level_data, created_at FROM saves WHERE account_id = ?", req.AccountId)
	if err := r2.Scan(&saveData, &levelData, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			// not found
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"saveData":            0,
				"levelData":           0,
				"totalSize":           0,
				"lastSaved":           "",
				"maxDataSize":         maxDataSize,
				"freeSpacePercentage": 100.0,
			})
			return
		}
		log.Error("check: save lookup error: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "Internal server error"})
		return
	}
	saveLen := 0
	levelLen := 0
	if saveData.Valid {
		saveLen = len(saveData.String)
	}
	if levelData.Valid {
		levelLen = len(levelData.String)
	}
	lastSaved := ""
	lastSavedRelative := ""
	if createdAt.Valid {
		lastSaved = createdAt.Time.Format(time.RFC3339)
		days := int(time.Since(createdAt.Time).Hours() / 24)
		switch days {
		case 0:
			lastSavedRelative = "today"
		case 1:
			lastSavedRelative = "1 day ago"
		default:
			lastSavedRelative = fmt.Sprintf("%d days ago", days)
		}
	}

	totalSize := saveLen + levelLen
	freeSpace := maxDataSize - totalSize
	if freeSpace < 0 {
		freeSpace = 0
	}
	freeSpacePercentage := float64(freeSpace) / float64(maxDataSize) * 100

	resp := struct {
		SaveData            int     `json:"saveData"`
		LevelData           int     `json:"levelData"`
		TotalSize           int     `json:"totalSize"`
		LastSaved           string  `json:"lastSaved"`
		LastSavedRelative   string  `json:"lastSavedRelative"`
		FreeSpacePercentage float64 `json:"freeSpacePercentage"`
		MaxDataSize         int     `json:"maxDataSize"`
	}{
		SaveData:            saveLen,
		LevelData:           levelLen,
		TotalSize:           totalSize,
		LastSaved:           lastSaved,
		LastSavedRelative:   lastSavedRelative,
		FreeSpacePercentage: freeSpacePercentage,
		MaxDataSize:         maxDataSize,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}
