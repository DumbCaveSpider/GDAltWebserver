package main

import (
    "context"
    "database/sql"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "time"
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
        http.Error(w, "-1", http.StatusMethodNotAllowed)
        log.Printf("check: invalid method %s", r.Method)
        return
    }
    body, err := io.ReadAll(r.Body)
    if err != nil {
        log.Printf("check: read body error: %v", err)
        http.Error(w, "-1", http.StatusBadRequest)
        return
    }
    var req CheckRequest
    if err := json.Unmarshal(body, &req); err != nil {
        log.Printf("check: json unmarshal error: %v", err)
        http.Error(w, "-1", http.StatusBadRequest)
        return
    }
    if req.AccountId == "" || req.ArgonToken == "" {
        log.Printf("check: missing accountId or argonToken")
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
        log.Printf("check: db open error: %v", err)
        http.Error(w, "-1", http.StatusInternalServerError)
        return
    }
    defer db.Close()
    if err := db.PingContext(ctx); err != nil {
        log.Printf("check: db ping error: %v", err)
        http.Error(w, "-1", http.StatusInternalServerError)
        return
    }

    // Verify account token
    var storedToken sql.NullString
    row := db.QueryRowContext(ctx, "SELECT argon_token FROM accounts WHERE account_id = ?", req.AccountId)
    switch err := row.Scan(&storedToken); err {
    case sql.ErrNoRows:
        http.Error(w, "-1", http.StatusForbidden)
        return
    case nil:
        if !storedToken.Valid || storedToken.String != req.ArgonToken {
            http.Error(w, "-1", http.StatusForbidden)
            return
        }
    default:
        log.Printf("check: account lookup error: %v", err)
        http.Error(w, "-1", http.StatusInternalServerError)
        return
    }

    // Get save_data length in bytes
    var saveData sql.NullString
    r2 := db.QueryRowContext(ctx, "SELECT save_data FROM saves WHERE account_id = ?", req.AccountId)
    if err := r2.Scan(&saveData); err != nil {
        if err == sql.ErrNoRows {
            // not found
            w.WriteHeader(http.StatusOK)
            _, _ = w.Write([]byte("0"))
            return
        }
        log.Printf("check: save lookup error: %v", err)
        http.Error(w, "-1", http.StatusInternalServerError)
        return
    }
    if !saveData.Valid {
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("0"))
        return
    }
    // Return byte length
    length := len(saveData.String)
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write([]byte(fmt.Sprintf("%d", length)))
}
