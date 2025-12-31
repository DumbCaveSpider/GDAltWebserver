package main

import (
	"context"
	"strings"
	"time"
)

func ensureMembershipsMigration() error {
	db, err := openDBConnection()
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createStmt := `CREATE TABLE IF NOT EXISTS memberships (
		id INT AUTO_INCREMENT PRIMARY KEY,
		kofi_transaction_id VARCHAR(255),
		email VARCHAR(255),
		discord_username VARCHAR(255),
		discord_userid VARCHAR(255),
		tier_name VARCHAR(255),
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`

	if _, err := db.ExecContext(ctx, createStmt); err != nil {
		return err
	}

	// Add tier_name column if it doesn't exist (in case table already existed without it)
	if _, err := db.ExecContext(ctx, "ALTER TABLE memberships ADD COLUMN tier_name VARCHAR(255)"); err != nil {
		if !strings.Contains(err.Error(), "Duplicate column name") && !strings.Contains(err.Error(), "exists") {
			return err
		}
	}

	// Add subscriber column to accounts if not exists - handled in ensureAccountsMigration

	return nil
}
