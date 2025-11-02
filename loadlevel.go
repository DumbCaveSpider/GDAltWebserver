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

func init() {
	http.HandleFunc("/loadlevel", loadLevelHandler)
}

func loadLevelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "-1", http.StatusMethodNotAllowed)
		log.Debug("loadlevel: invalid method %s", r.Method)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("loadlevel: read body error: %v", err)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	var req LoadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Error("loadlevel: json unmarshal error: %v", err)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}
	if req.AccountId == "" || req.ArgonToken == "" {
		log.Warn("loadlevel: missing accountId or argonToken")
		http.Error(w, "-1", http.StatusBadRequest)
		return
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
		log.Error("loadlevel: db open error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Error("loadlevel: db ping error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("loadlevel: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Warn("loadlevel: token validation failed for %s", req.AccountId)
		http.Error(w, "-1", http.StatusForbidden)
		return
	}

	var levelData sql.NullString
	r2 := db.QueryRowContext(ctx, "SELECT level_data FROM saves WHERE account_id = ?", req.AccountId)
	if err := r2.Scan(&levelData); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "-1", http.StatusNotFound)
			return
		}
		log.Error("loadlevel: save lookup error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	if !levelData.Valid {
		http.Error(w, "-1", http.StatusNotFound)
		return
	}

	// Decompress the level data
	decompressed, err := decompressLevelData(levelData.String)
	if err != nil {
		log.Error("loadlevel: decompression error for %s: %v", req.AccountId, err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	log.Info("loadlevel: decompressed level data from %d to %d bytes for %s",
		len(levelData.String), len(decompressed), req.AccountId)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(decompressed))
}

// decompressLevelData decompresses base64-encoded gzip data, with fallback for uncompressed data
func decompressLevelData(data string) (string, error) {
	if data == "" {
		return "", nil
	}

	isCompressed := !strings.Contains(data, "<") && !strings.Contains(data, ">")

	if !isCompressed {
		log.Debug("loadlevel: data appears uncompressed, returning as-is")
		return data, nil
	}

	// Decode base64
	compressedBytes, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		log.Warn("loadlevel: base64 decode failed, treating as uncompressed: %v", err)
		return data, nil
	}

	// Decompress gzip
	gzReader, err := gzip.NewReader(bytes.NewReader(compressedBytes))
	if err != nil {
		log.Warn("loadlevel: gzip reader failed, treating as uncompressed: %v", err)
		return data, nil
	}
	defer gzReader.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, gzReader); err != nil {
		return "", fmt.Errorf("gzip read error: %w", err)
	}

	return buf.String(), nil
}
