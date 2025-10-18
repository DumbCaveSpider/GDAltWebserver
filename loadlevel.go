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

// Reuse LoadRequest type defined in load.go

func init() {
    http.HandleFunc("/loadlevel", loadLevelHandler)
}

func loadLevelHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "-1", http.StatusMethodNotAllowed)
        log.Printf("loadlevel: invalid method %s", r.Method)
        return
    }

    body, err := io.ReadAll(r.Body)
    if err != nil {
        log.Printf("loadlevel: read body error: %v", err)
        http.Error(w, "-1", http.StatusBadRequest)
        return
    }

    var req LoadRequest
    if err := json.Unmarshal(body, &req); err != nil {
        log.Printf("loadlevel: json unmarshal error: %v", err)
        http.Error(w, "-1", http.StatusBadRequest)
        return
    }
    if req.AccountId == "" || req.ArgonToken == "" {
        log.Printf("loadlevel: missing accountId or argonToken")
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
        log.Printf("loadlevel: db open error: %v", err)
        http.Error(w, "-1", http.StatusInternalServerError)
        return
    }
    defer db.Close()

    if err := db.PingContext(ctx); err != nil {
        log.Printf("loadlevel: db ping error: %v", err)
        http.Error(w, "-1", http.StatusInternalServerError)
        return
    }

    // Validate token using Argon helper
    ok, verr := ValidateArgonToken(ctx, db, req.AccountId, req.ArgonToken)
    if verr != nil {
        log.Printf("loadlevel: token validation error for %s: %v", req.AccountId, verr)
        http.Error(w, "-1", http.StatusInternalServerError)
        return
    }
    if !ok {
        log.Printf("loadlevel: token validation failed for %s", req.AccountId)
        http.Error(w, "-1", http.StatusForbidden)
        return
    }

    // Fetch level_data from central saves table
    var levelData sql.NullString
    r2 := db.QueryRowContext(ctx, "SELECT level_data FROM saves WHERE account_id = ?", req.AccountId)
    if err := r2.Scan(&levelData); err != nil {
        if err == sql.ErrNoRows {
            // no save found
            http.Error(w, "-1", http.StatusNotFound)
            return
        }
        log.Printf("loadlevel: save lookup error: %v", err)
        http.Error(w, "-1", http.StatusInternalServerError)
        return
    }

    if !levelData.Valid {
        http.Error(w, "-1", http.StatusNotFound)
        return
    }

    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write([]byte(levelData.String))
}
