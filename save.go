package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/DumbCaveSpider/GDAlternativeWeb/log"

	_ "github.com/go-sql-driver/mysql"
)

type SaveRequest struct {
	AccountId  string `json:"accountId"`
	SaveData   string `json:"saveData"`
	LevelData  string `json:"levelData"`
	ArgonToken string `json:"argonToken"`
}

func (s *SaveRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	getStr := func(keys ...string) string {
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

	s.AccountId = getStr("accountId", "account_id")
	s.SaveData = getStr("saveData", "save_data")
	s.LevelData = getStr("levelData", "level_data")
	s.ArgonToken = getStr("argonToken", "argon_token")
	return nil
}

func init() {
	http.HandleFunc("/save", saveHandler)
}

func saveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "-1", http.StatusMethodNotAllowed)
		log.Warn("save: invalid method %s", r.Method)
		return
	}

	var req SaveRequest

	body, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		log.Warn("save: read body error: %v", readErr)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	if err := json.Unmarshal(body, &req); err != nil {
		bodyPreview := redactPreview(string(body), 200)
		log.Warn("save: json unmarshal error: %v content-type=%s bodyPreview=%s", err, r.Header.Get("Content-Type"), bodyPreview)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	savePreview := redactPreview(req.SaveData, 120)
	levelPreview := redactPreview(req.LevelData, 120)
	argonPreview := redactPreview(req.ArgonToken, 80)
	log.Debug("save: parsed body as JSON (accountId='%s', saveDataPreview='%s', levelDataPreview='%s', argonTokenPreview='%s')", req.AccountId, savePreview, levelPreview, argonPreview)
	if req.AccountId == "" || req.ArgonToken == "" || (req.SaveData == "" && req.LevelData == "") {
		ct := r.Header.Get("Content-Type")
		bodyPreview := redactPreview(string(body), 200)
		log.Warn("save: missing accountId/argonToken or neither saveData nor levelData provided (accountId='%s', saveDataPresent=%v, levelDataPresent=%v, argonTokenPresent=%v) content-type=%s bodyPreview=%s", req.AccountId, req.SaveData != "", req.LevelData != "", req.ArgonToken != "", ct, bodyPreview)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	// hard limit 32 MiB
	maxLevelDataSize := 33554432
	if v := os.Getenv("MAX_LEVEL_DATA_SIZE_BYTES"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			maxLevelDataSize = parsed
		}
	}
	if len(req.LevelData) > maxLevelDataSize {
		log.Warn("save: levelData size %d exceeds hard limit of %d bytes", len(req.LevelData), maxLevelDataSize)
		http.Error(w, "-1", http.StatusRequestEntityTooLarge)
		return
	}

	maxAccountDataSize := 33554432 // 32 MiB
	if v := os.Getenv("MAX_ACCOUNT_DATA_SIZE_BYTES"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			maxAccountDataSize = parsed
		}
	}
	if len(req.SaveData) > maxAccountDataSize {
		log.Warn("save: saveData size %d exceeds hard limit of %d bytes", len(req.SaveData), maxAccountDataSize)
		http.Error(w, "-1", http.StatusRequestEntityTooLarge)
		return
	}

	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	if dbUser == "" || dbHost == "" || dbName == "" {
		log.Error("save: missing DB_* env vars (DB_USER, DB_HOST, DB_NAME required)")
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if dbPort == "" {
		dbPort = "3306"
	}

	dbMaxAllowedPacket := os.Getenv("DB_MAX_ALLOWED_PACKET")
	if dbMaxAllowedPacket == "" {
		dbMaxAllowedPacket = "1073741824"
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&maxAllowedPacket=%s&timeout=30s&readTimeout=30s&writeTimeout=60s",
		dbUser, dbPass, dbHost, dbPort, dbName, dbMaxAllowedPacket)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Error("save: db open error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	defer db.Close()

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		log.Error("save: db ping error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	createStmt := `CREATE TABLE IF NOT EXISTS saves (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		account_id VARCHAR(255) NOT NULL,
		save_data LONGTEXT NOT NULL,
		level_data LONGTEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE KEY unique_account (account_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, createStmt); err != nil {
		log.Error("save: create table error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	acctCreate := `CREATE TABLE IF NOT EXISTS accounts (
		account_id VARCHAR(255) PRIMARY KEY,
		argon_token VARCHAR(512) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, acctCreate); err != nil {
		log.Error("save: create accounts table error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	var storedToken sql.NullString
	row := db.QueryRowContext(ctx, "SELECT argon_token FROM accounts WHERE account_id = ?", req.AccountId)
	switch err := row.Scan(&storedToken); err {
	case sql.ErrNoRows:
		log.Error("save: init POST for new account %s from %s", req.AccountId, r.RemoteAddr)
		if _, err := execWithRetries(ctx, db, "INSERT INTO accounts (account_id, argon_token) VALUES (?, ?)", req.AccountId, req.ArgonToken); err != nil {
			log.Error("save: insert account error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
	case nil:
	default:
		log.Error("save: account lookup error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
	if verr != nil {
		log.Error("save: token validation error for %s: %v", req.AccountId, verr)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if !ok {
		log.Warn("save: token validation failed for %s", req.AccountId)
		http.Error(w, "-1", http.StatusForbidden)
		return
	}

	// Ensure row exists with empty data if not present
	//INSERT IGNORE so it does nothing if the row already exists.
	// This splits the operation: first ensure row, then update columns separately.
	ensureStmt := "INSERT IGNORE INTO saves (account_id, save_data, level_data) VALUES (?, '', '')"
	if _, err := execWithRetries(ctx, db, ensureStmt, req.AccountId); err != nil {
		log.Error("save: ensure row error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Update save_data if present
	// Check server's max_allowed_packet
	var maxAllowedPacket int
	if err := db.QueryRowContext(ctx, "SELECT @@max_allowed_packet").Scan(&maxAllowedPacket); err != nil {
		log.Warn("save: could not query max_allowed_packet: %v", err)
		maxAllowedPacket = 4194304 // 4MB default fallback
	} else {
		log.Debug("save: server max_allowed_packet is %d bytes", maxAllowedPacket)
	}

	// Update save_data if present
	if req.SaveData != "" {
		if len(req.SaveData) > maxAllowedPacket {
			log.Error("save: save_data size %d exceeds server max_allowed_packet %d", len(req.SaveData), maxAllowedPacket)
			http.Error(w, "-1", http.StatusRequestEntityTooLarge)
			return
		}
		updateSave := "UPDATE saves SET save_data = ?, created_at = CURRENT_TIMESTAMP WHERE account_id = ?"
		if _, err := execWithRetries(ctx, db, updateSave, req.SaveData, req.AccountId); err != nil {
			log.Error("save: update save_data error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
	}

	// Update level_data if present
	if req.LevelData != "" {
		if len(req.LevelData) > maxAllowedPacket {
			log.Error("save: level_data size %d exceeds server max_allowed_packet %d", len(req.LevelData), maxAllowedPacket)
			http.Error(w, "-1", http.StatusRequestEntityTooLarge)
			return
		}
		log.Debug("save: updating level_data (size=%d)", len(req.LevelData))
		updateLevel := "UPDATE saves SET level_data = ?, created_at = CURRENT_TIMESTAMP WHERE account_id = ?"
		if _, err := execWithRetries(ctx, db, updateLevel, req.LevelData, req.AccountId); err != nil {
			log.Error("save: update level_data error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}

func redactPreview(s string, maxLen int) string {
	if s == "" {
		return "(empty)"
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	for i, p := range parts {
		if len(p) > 50 {
			if len(p) > 20 {
				parts[i] = p[:10] + "..." + p[len(p)-10:]
			} else {
				parts[i] = p[:10] + "..."
			}
		}
	}
	joined := strings.Join(parts, " ")
	if len(joined) > maxLen {
		return joined[:maxLen] + "..."
	}
	return joined
}

func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection reset by peer"):
		return true
	case strings.Contains(msg, "broken pipe"):
		return true
	case strings.Contains(msg, "i/o timeout"):
		return true
	case strings.Contains(msg, "connection refused"):
		return true
	case strings.Contains(msg, "tls: handshake timeout"):
		return true
	case strings.Contains(msg, "use of closed network connection"):
		return true
	}
	return false
}

func execWithRetries(ctx context.Context, db *sql.DB, query string, args ...interface{}) (sql.Result, error) {
	var res sql.Result
	var err error
	backoff := 200 * time.Millisecond
	for attempt := 1; attempt <= 3; attempt++ {
		res, err = db.ExecContext(ctx, query, args...)
		if err == nil {
			return res, nil
		}
		if isTransient(err) && attempt < 3 {
			log.Debug("save: transient db error (attempt %d): %v; retrying after %s", attempt, err, backoff)
			select {
			case <-time.After(backoff):
				backoff *= 2
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		break
	}
	return res, err
}
