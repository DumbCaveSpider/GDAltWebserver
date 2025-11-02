package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
		http.Error(w, "-1", http.StatusMethodNotAllowed)
		log.Debug("load: invalid method %s", r.Method)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("load: read body error: %v", err)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	var req LoadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// try fallback simple parsing (urlencoded not necessary here)
		log.Warn("load: json unmarshal error: %v", err)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("load: missing accountId or argonToken")
		http.Error(w, "-1", http.StatusBadRequest)
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
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Error("load: db ping error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Validate token using Argon helper
	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("load: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Warn("load: token validation failed for %s", req.AccountId)
		http.Error(w, "-1", http.StatusForbidden)
		return
	}

	// Fetch save_data from central saves table
	var saveData sql.NullString
	r2 := db.QueryRowContext(ctx, "SELECT save_data FROM saves WHERE account_id = ?", req.AccountId)
	if err := r2.Scan(&saveData); err != nil {
		if err == sql.ErrNoRows {
			// no save found
			http.Error(w, "-1", http.StatusNotFound)
			return
		}
		log.Error("load: save lookup error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	if !saveData.Valid {
		http.Error(w, "-1", http.StatusNotFound)
		return
	}

	// Decompress the save data
	decompressed, err := decompressSaveData(saveData.String)
	if err != nil {
		log.Error("load: decompression error for %s: %v", req.AccountId, err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	log.Info("load: decompressed save data from %d to %d bytes for %s",
		len(saveData.String), len(decompressed), req.AccountId)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(decompressed))
}

// decompressSaveData decompresses base64-encoded gzip data, with fallback for uncompressed data
func decompressSaveData(data string) (string, error) {
	if data == "" {
		return "", nil
	}

	// Try to detect if data is compressed (base64 gzip) or plain text
	// Base64 only contains alphanumeric + / + = characters
	// GD save data contains < > and other XML-like characters
	isCompressed := !strings.Contains(data, "<") && !strings.Contains(data, ">")

	if !isCompressed {
		// Data appears to be uncompressed (legacy data)
		log.Debug("load: data appears uncompressed, returning as-is")
		return data, nil
	}

	// Decode base64
	compressedBytes, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		// If base64 decode fails, assume it's uncompressed
		log.Warn("load: base64 decode failed, treating as uncompressed: %v", err)
		return data, nil
	}

	// Decompress gzip
	gzReader, err := gzip.NewReader(bytes.NewReader(compressedBytes))
	if err != nil {
		// If gzip read fails, assume it's uncompressed
		log.Warn("load: gzip reader failed, treating as uncompressed: %v", err)
		return data, nil
	}
	defer gzReader.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, gzReader); err != nil {
		return "", fmt.Errorf("gzip read error: %w", err)
	}

	return buf.String(), nil
}
