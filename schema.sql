-- Accounts table
CREATE TABLE IF NOT EXISTS accounts (
    account_id VARCHAR(255) PRIMARY KEY,
    argon_token VARCHAR(512) NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    token_validated_at TIMESTAMP NULL,
    subscriber BOOLEAN DEFAULT FALSE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Saves table
CREATE TABLE IF NOT EXISTS saves (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    account_id VARCHAR(255) NOT NULL,
    save_data LONGTEXT NOT NULL,
    level_data LONGTEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY unique_account (account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Memberships table
CREATE TABLE IF NOT EXISTS memberships (
    id INT AUTO_INCREMENT PRIMARY KEY,
    kofi_transaction_id VARCHAR(255),
    email VARCHAR(255),
    discord_username VARCHAR(255),
    discord_userid VARCHAR(255),
    tier_name VARCHAR(255),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
