package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
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

	maxAccountDataSize := 16777216 // 16 MiB
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

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&maxAllowedPacket=67108864&timeout=30s&readTimeout=30s&writeTimeout=30s",
		dbUser, dbPass, dbHost, dbPort, dbName)

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

	// Compress save data before storing
	var compressedSaveData string
	if req.SaveData != "" {
		compressed, err := compressData(req.SaveData)
		if err != nil {
			log.Error("save: save data compression error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
		compressedSaveData = compressed
		log.Info("save: compressed save data from %d to %d bytes (%.1f%% of original)",
			len(req.SaveData), len(compressedSaveData),
			float64(len(compressedSaveData))/float64(len(req.SaveData))*100)
	}

	// Compress level data before storing
	var compressedLevelData string
	if req.LevelData != "" {
		compressed, err := compressData(req.LevelData)
		if err != nil {
			log.Error("save: level data compression error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
		compressedLevelData = compressed
		log.Info("save: compressed level data from %d to %d bytes (%.1f%% of original)",
			len(req.LevelData), len(compressedLevelData),
			float64(len(compressedLevelData))/float64(len(req.LevelData))*100)
	}

	var updateQuery string
	var updateArgs []interface{}
	var setParts []string
	if req.SaveData != "" {
		setParts = append(setParts, "save_data = ?")
		updateArgs = append(updateArgs, compressedSaveData)
	}
	if req.LevelData != "" {
		setParts = append(setParts, "level_data = ?")
		updateArgs = append(updateArgs, compressedLevelData)
	}
	setParts = append(setParts, "created_at = CURRENT_TIMESTAMP")
	updateQuery = fmt.Sprintf("UPDATE saves SET %s WHERE account_id = ?", strings.Join(setParts, ", "))
	updateArgs = append(updateArgs, req.AccountId)

	res, err := execWithRetries(ctx, db, updateQuery, updateArgs...)
	if err != nil {
		saveSize := len(req.SaveData)
		levelSize := len(req.LevelData)
		totalSize := saveSize + levelSize
		stats := db.Stats()
		log.Error("save: update central save error: %v", err)
		log.Debug("save: payload sizes: save=%d bytes level=%d bytes total=%d bytes", saveSize, levelSize, totalSize)
		log.Debug("save: db stats: OpenConnections=%d InUse=%d Idle=%d WaitCount=%d MaxOpenConnections=%d", stats.OpenConnections, stats.InUse, stats.Idle, stats.WaitCount, stats.MaxOpenConnections)

		if _, perr := execWithRetries(ctx, db, "SET @probe = 1"); perr != nil {
			log.Error("save: small probe write failed: %v", perr)
		} else {
			log.Info("save: small probe write succeeded — large payload may be the cause")
		}

		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		saveVal := compressedSaveData
		levelVal := compressedLevelData
		if saveVal == "" {
			saveVal = ""
		}
		if levelVal == "" {
			levelVal = ""
		}
		if _, err := execWithRetries(ctx, db, "INSERT INTO saves (account_id, save_data, level_data) VALUES (?, ?, ?)", req.AccountId, saveVal, levelVal); err != nil {
			log.Error("save: insert central save error: %v", err)
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

func compressData(data string) (string, error) {
	if data == "" {
		return "", nil
	}

	dataToCompress := data
	wasBase64 := false

	if !strings.Contains(data, "<") && !strings.Contains(data, ">") && len(data) > 100 {
		if decoded, err := base64.StdEncoding.DecodeString(data); err == nil {
			dataToCompress = string(decoded)
			wasBase64 = true
			log.Debug("compress: detected base64 input, decoding before compression (%d -> %d bytes)",
				len(data), len(dataToCompress))
		}
	}

	var buf bytes.Buffer
	gzWriter, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", fmt.Errorf("gzip writer init error: %w", err)
	}

	if _, err := gzWriter.Write([]byte(dataToCompress)); err != nil {
		gzWriter.Close()
		return "", fmt.Errorf("gzip write error: %w", err)
	}

	if err := gzWriter.Close(); err != nil {
		return "", fmt.Errorf("gzip close error: %w", err)
	}

	compressedBytes := buf.Bytes()

	prefix := ""
	if wasBase64 {
		prefix = "B64GZ:"
	} else {
		prefix = "GZ:"
	}
	compressed := prefix + base64.StdEncoding.EncodeToString(compressedBytes)

	originalSize := len(data)
	compressedBinarySize := len(compressedBytes)
	compressedBase64Size := len(compressed)
	ratio := float64(compressedBinarySize) / float64(originalSize) * 100
	log.Debug("compress: original=%d bytes gzip=%d bytes final=%d bytes ratio=%.1f%% wasBase64=%v",
		originalSize, compressedBinarySize, compressedBase64Size, ratio, wasBase64)

	//doesn't save at least 10%, don't use it
	if compressedBase64Size >= int(float64(originalSize)*0.90) {
		log.Warn("compress: compression ineffective (%.1f%%), storing uncompressed",
			float64(compressedBase64Size)/float64(originalSize)*100)
		return data, nil
	}

	return compressed, nil
}
