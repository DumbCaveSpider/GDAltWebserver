package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type SaveRequest struct {
	AccountId  string `json:"accountId"`
	SaveData   string `json:"saveData"`
	ArgonToken string `json:"argonToken"`
}

// UnmarshalJSON implements a tolerant JSON unmarshaller for SaveRequest.
// It accepts accountId as a number or string and recognizes both
// "accountId" and "accountID" keys.
func (s *SaveRequest) UnmarshalJSON(data []byte) error {
	// Decode into a generic map first
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Helper to fetch a string from possible key variations
	getStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil {
				switch t := v.(type) {
				case string:
					return t
				case float64:
					// JSON numbers decode as float64 — format without decimals
					return fmt.Sprintf("%.0f", t)
				case json.Number:
					return t.String()
				default:
					// fallback to string conversion
					return fmt.Sprintf("%v", t)
				}
			}
		}
		return ""
	}

	s.AccountId = getStr("accountId", "account_id")
	s.SaveData = getStr("saveData", "save_data")
	s.ArgonToken = getStr("argonToken", "argon_token")
	return nil
}

func init() {
	// register handler on the default mux used by main.go
	http.HandleFunc("/save", saveHandler)
}

func saveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "-1", http.StatusMethodNotAllowed)
		log.Printf("save: invalid method %s", r.Method)
		return
	}

	var req SaveRequest

	// Read the full request body so we can attempt multiple parsing strategies
	body, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		log.Printf("save: read body error: %v", readErr)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	// Try JSON first
	if err := json.Unmarshal(body, &req); err != nil {
		// JSON failed — log and try fallbacks
		log.Printf("save: json unmarshal error: %v — attempting fallbacks (body len=%d)", err, len(body))

		// 1) Try parsing as urlencoded form data
		if vals, perr := url.ParseQuery(string(body)); perr == nil && len(vals) > 0 {
			// Accept form fields: accountId, saveData, argonToken
			if v := vals.Get("accountId"); v != "" {
				req.AccountId = v
			}
			req.SaveData = vals.Get("saveData")
			req.ArgonToken = vals.Get("argonToken")
			log.Printf("save: parsed body as urlencoded form (keys: %v)", strings.Join(keysFromValues(vals), ","))
		} else {
			// 2) Try plain key:value lines (e.g. "accountId:7689052\nsaveData:...\n")
			text := strings.TrimSpace(string(body))
			if text != "" {
				lines := strings.Split(text, "\n")
				for _, line := range lines {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					// Accept both key:value and key=value
					var parts []string
					if idx := strings.Index(line, ":"); idx >= 0 {
						parts = []string{strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:])}
					} else if idx := strings.Index(line, "="); idx >= 0 {
						parts = []string{strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:])}
					} else {
						continue
					}
					key := strings.ToLower(parts[0])
					val := parts[1]
					switch key {
					case "accountid", "account_id":
						req.AccountId = val
					case "savedata", "save_data":
						req.SaveData = val
					case "argontoken", "argon_token":
						req.ArgonToken = val
					}
				}
				log.Printf("save: parsed body as plain lines")
			}
		}
	} else {
		// JSON unmarshal succeeded
		log.Printf("save: parsed body as JSON (accountId='%s', saveData len=%d, argonToken len=%d)", req.AccountId, len(req.SaveData), len(req.ArgonToken))
	}
	if req.AccountId == "" || req.SaveData == "" || req.ArgonToken == "" {
		// Log useful debugging info: content-type, length, and a short
		// redacted preview of the body so we can see what the client sent
		// without leaking full secrets.
		ct := r.Header.Get("Content-Type")
		bodyPreview := redactPreview(string(body), 200)
		log.Printf("save: missing accountId, saveData or argonToken (accountId='%s', saveDataPresent=%v, argonTokenPresent=%v) content-type=%s bodyPreview=%s", req.AccountId, req.SaveData != "", req.ArgonToken != "", ct, bodyPreview)
		http.Error(w, "-1", http.StatusBadRequest)
		return
	}

	// Build DSN from environment variables
	dbUser := os.Getenv("DB_USER")
	dbPass := os.Getenv("DB_PASS")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	if dbUser == "" || dbHost == "" || dbName == "" {
		log.Printf("save: missing DB_* env vars (DB_USER, DB_HOST, DB_NAME required)")
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	if dbPort == "" {
		dbPort = "3306"
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4", dbUser, dbPass, dbHost, dbPort, dbName)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Printf("save: db open error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Printf("save: db ping error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Ensure central saves table exists
	createStmt := `CREATE TABLE IF NOT EXISTS saves (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		account_id VARCHAR(255) NOT NULL,
		save_data LONGTEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, createStmt); err != nil {
		log.Printf("save: create table error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Ensure accounts table exists in central DB
	acctCreate := `CREATE TABLE IF NOT EXISTS accounts (
		account_id VARCHAR(255) PRIMARY KEY,
		argon_token VARCHAR(512) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`
	if _, err := db.ExecContext(ctx, acctCreate); err != nil {
		log.Printf("save: create accounts table error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Helper to sanitize database name (allow alnum and underscore, otherwise sha1)
	sanitize := func(s string) string {
		re := regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
		if re.MatchString(s) {
			return s
		}
		h := sha1.Sum([]byte(s))
		return hex.EncodeToString(h[:])
	}
	accDbName := "acct_" + sanitize(req.AccountId)

	// Check if account exists in accounts table
	var storedToken sql.NullString
	// perAcctTarget records where to insert per-account saves. It may be
	// "`<db>`.saves" or a central table name like "`saves_<id>`".
	var perAcctTarget string
	row := db.QueryRowContext(ctx, "SELECT argon_token FROM accounts WHERE account_id = ?", req.AccountId)
	switch err := row.Scan(&storedToken); err {
	case sql.ErrNoRows:
		// First time account: log an init POST (don't log secrets)
		log.Printf("save: init POST for new account %s from %s", req.AccountId, r.RemoteAddr)
		// First time account: insert into accounts and attempt to create per-account database
		if _, err := db.ExecContext(ctx, "INSERT INTO accounts (account_id, argon_token) VALUES (?, ?)", req.AccountId, req.ArgonToken); err != nil {
			log.Printf("save: insert account error: %v", err)
			http.Error(w, "-1", http.StatusInternalServerError)
			return
		}
		// Try to create a separate per-account database. If the DB user
		// lacks permission to CREATE DATABASE (common on managed hosts),
		// fall back to creating a per-account table in the central DB.
		perAcctTarget = "" // SQL target for inserts (e.g. `acct_x`.saves or `saves_acct_x`)

		createDbStmt := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;", accDbName)
		if _, err := db.ExecContext(ctx, createDbStmt); err != nil {
			// If access denied to create databases, fall back to central table
			log.Printf("save: create account database error: %v — will fall back to central per-account table if permitted", err)
			if strings.Contains(err.Error(), "Access denied") || strings.Contains(err.Error(), "1044") {
				// create a per-account table in the central DB named saves_<sanitized>
				tbl := "saves_" + sanitize(req.AccountId)
				perAcctTarget = fmt.Sprintf("`%s`", tbl)
				createPerAcctCentral := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s ( id BIGINT AUTO_INCREMENT PRIMARY KEY, account_id VARCHAR(255) NOT NULL, save_data LONGTEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;", perAcctTarget)
				if _, err := db.ExecContext(ctx, createPerAcctCentral); err != nil {
					log.Printf("save: create central per-account table error: %v", err)
					http.Error(w, "-1", http.StatusInternalServerError)
					return
				}
			} else {
				log.Printf("save: create account database error: %v", err)
				http.Error(w, "-1", http.StatusInternalServerError)
				return
			}
		} else {
			// Created (or ensured) per-account database; create its saves table
			createPerAcct := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s`.saves ( id BIGINT AUTO_INCREMENT PRIMARY KEY, account_id VARCHAR(255) NOT NULL, save_data LONGTEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;", accDbName)
			if _, err := db.ExecContext(ctx, createPerAcct); err != nil {
				log.Printf("save: create per-account saves table error: %v", err)
				http.Error(w, "-1", http.StatusInternalServerError)
				return
			}
			perAcctTarget = fmt.Sprintf("`%s`.saves", accDbName)
		}
	case nil:
		// existing account: verify token matches
		if !storedToken.Valid || storedToken.String != req.ArgonToken {
			log.Printf("save: argon token mismatch for account %s", req.AccountId)
			http.Error(w, "-1", http.StatusForbidden)
			return
		}
	default:
		log.Printf("save: account lookup error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Insert the save into central saves table
	if _, err := db.ExecContext(ctx, "INSERT INTO saves (account_id, save_data) VALUES (?, ?)", req.AccountId, req.SaveData); err != nil {
		log.Printf("save: insert central save error: %v", err)
		http.Error(w, "-1", http.StatusInternalServerError)
		return
	}

	// Also insert into per-account DB/table saves table (if exists). Use
	// perAcctTarget (set earlier) which is either "`db`.saves" or a
	// central table name like "`saves_<id>`".
	target := "`" + accDbName + "`.saves"
	if perAcctTarget != "" {
		// perAcctTarget may be "`db`.saves" or "`saves_<id>`"
		if strings.Contains(perAcctTarget, ".") {
			target = perAcctTarget
		} else {
			// central table case
			target = perAcctTarget
		}
	}
	perAcctInsert := fmt.Sprintf("INSERT INTO %s (account_id, save_data) VALUES (?, ?)", target)
	if _, err := db.ExecContext(ctx, perAcctInsert, req.AccountId, req.SaveData); err != nil {
		log.Printf("save: insert per-account save error (continuing): %v", err)
		// don't fail the whole request for per-account insert failure — we already inserted centrally
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("1"))
}

// keysFromValues returns the list of keys present in url.Values
func keysFromValues(v url.Values) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	return keys
}

// redactPreview returns a short preview of s (up to maxLen) and masks
// any long token-like substrings (e.g. argonToken) to avoid leaking
// secrets in logs.
func redactPreview(s string, maxLen int) string {
	if s == "" {
		return "(empty)"
	}
	// Mask common token-like patterns: long dot-separated tokens
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	for i, p := range parts {
		if len(p) > 50 {
			// shorten and mask the middle
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
