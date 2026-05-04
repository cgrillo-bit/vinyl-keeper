package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"strings"
)

const canonicalLanceUserID int64 = 0

func (k *keeper) MigrateLegacyUserVinylPlays() error {
	if k.db == nil {
		return fmt.Errorf("database not initialized")
	}

	dbPath, err := databasePath()
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	backupPath := filepath.Join(filepath.Dir(dbPath), "_vinylkeeper.db")

	if err := ensureCanonicalLanceUser(k.ctx, k.db); err != nil {
		return err
	}

	tx, err := k.db.BeginTx(k.ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy plays migration tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(k.ctx, "ATTACH DATABASE ? AS backup", backupPath); err != nil {
		return fmt.Errorf("attach backup db %q: %w", backupPath, err)
	}
	defer func() {
		_, _ = tx.ExecContext(k.ctx, "DETACH DATABASE backup")
	}()

	exists, err := tableExistsInDBTx(k.ctx, tx, "backup", "user_vinyl_plays")
	if err != nil {
		return err
	}
	if !exists {
		log.Printf("[migration] backup.user_vinyl_plays not found, skipping play migration")
		return nil
	}

	rows, err := tx.QueryContext(
		k.ctx,
		"SELECT user_id, vinyl_id, plays, first_played, last_played FROM backup.user_vinyl_plays ORDER BY user_id, vinyl_id",
	)
	if err != nil {
		return fmt.Errorf("query backup.user_vinyl_plays: %w", err)
	}
	defer rows.Close()

	total := 0
	migrated := 0
	failed := 0

	for rows.Next() {
		total++

		var userID int64
		var vinylID int64
		var plays int64
		var firstPlayed sql.NullString
		var lastPlayed sql.NullString
		if err := rows.Scan(&userID, &vinylID, &plays, &firstPlayed, &lastPlayed); err != nil {
			failed++
			log.Printf("[migration] legacy row scan failed: %v", err)
			continue
		}

		releaseID, err := lookupPrimaryReleaseID(k.ctx, tx, vinylID)
		if err != nil {
			failed++
			log.Printf("[migration] missing primary release for vinyl_id=%d: %v", vinylID, err)
			continue
		}

		initialDate := normalizeDate(firstPlayed, lastPlayed)
		lastDate := normalizeDate(lastPlayed, firstPlayed)
		targetUserID := canonicalLanceUserID
		if userID != canonicalLanceUserID {
			log.Printf("[migration] remap legacy user_id=%d -> user_id=%d for vinyl_id=%d", userID, targetUserID, vinylID)
		}

		if _, err := tx.ExecContext(
			k.ctx,
			"INSERT OR IGNORE INTO vinyl_plays(user_id, vinyl_id, release_id, play, played_date) VALUES (?, ?, ?, 0, ?)",
			targetUserID,
			vinylID,
			releaseID,
			initialDate,
		); err != nil {
			failed++
			log.Printf("[migration] insert play=0 failed user_id=%d vinyl_id=%d: %v", targetUserID, vinylID, err)
			continue
		}

		if _, err := tx.ExecContext(
			k.ctx,
			"INSERT OR IGNORE INTO vinyl_plays(user_id, vinyl_id, release_id, play, played_date) VALUES (?, ?, ?, 1, ?)",
			targetUserID,
			vinylID,
			releaseID,
			initialDate,
		); err != nil {
			failed++
			log.Printf("[migration] insert play=1 failed user_id=%d vinyl_id=%d: %v", targetUserID, vinylID, err)
			continue
		}

		if plays >= 2 {
			if _, err := tx.ExecContext(
				k.ctx,
				"INSERT OR IGNORE INTO vinyl_plays(user_id, vinyl_id, release_id, play, played_date) VALUES (?, ?, ?, 2, ?)",
				targetUserID,
				vinylID,
				releaseID,
				lastDate,
			); err != nil {
				failed++
				log.Printf("[migration] insert play=2 failed user_id=%d vinyl_id=%d: %v", targetUserID, vinylID, err)
				continue
			}
		}

		migrated++
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy user_vinyl_plays: %w", err)
	}

	if failed > 0 {
		log.Printf("[migration] legacy plays failed rows=%d of %d; preserving backup.user_vinyl_plays for retry", failed, total)
		return fmt.Errorf("legacy plays migration completed with failures: %d of %d rows failed", failed, total)
	}
	log.Printf("[migration] preserving backup.user_vinyl_plays source table")

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy plays migration tx: %w", err)
	}

	log.Printf("[migration] legacy plays done: total=%d migrated=%d failed=%d", total, migrated, failed)

	return nil
}

func normalizeDate(primary, fallback sql.NullString) string {
	if primary.Valid {
		v := strings.TrimSpace(primary.String)
		if v != "" {
			return v
		}
	}
	if fallback.Valid {
		v := strings.TrimSpace(fallback.String)
		if v != "" {
			return v
		}
	}
	return "1970-01-01"
}

func lookupPrimaryReleaseID(ctx context.Context, tx *sql.Tx, vinylID int64) (int64, error) {
	row := tx.QueryRowContext(ctx, "SELECT release_id FROM vinyl_release WHERE vinyl_id = ? AND master_release = 1 LIMIT 1", vinylID)
	var releaseID int64
	if err := row.Scan(&releaseID); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("no master release row")
		}
		return 0, err
	}
	return releaseID, nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	row := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)", name)
	var exists int64
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("check table %s exists: %w", name, err)
	}
	return exists == 1, nil
}

func tableExistsInDBTx(ctx context.Context, tx *sql.Tx, dbAlias, name string) (bool, error) {
	query := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s.sqlite_master WHERE type = 'table' AND name = ?)", dbAlias)
	row := tx.QueryRowContext(ctx, query, name)
	var exists int64
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("check table %s.%s exists: %w", dbAlias, name, err)
	}
	return exists == 1, nil
}

func ensureCanonicalLanceUser(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "INSERT OR IGNORE INTO users(user_id, user_name) VALUES (?, ?)", canonicalLanceUserID, "Lance"); err != nil {
		return fmt.Errorf("ensure canonical Lance user: %w", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE users SET user_name = ? WHERE user_id = ?", "Lance", canonicalLanceUserID); err != nil {
		return fmt.Errorf("update canonical Lance user name: %w", err)
	}
	return nil
}
