package service

import (
	"database/sql"
	"fmt"
	"time"
)

// ViewsService records per-user open counts for assets.
type ViewsService struct {
	db *sql.DB
}

func NewViewsService(db *sql.DB) *ViewsService {
	return &ViewsService{db: db}
}

// Record increments the view counter for (userID, assetID), creating the row on
// first view. Returns ErrNotFound when the asset does not exist.
func (s *ViewsService) Record(userID, assetID string) error {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM assets WHERE id=?`, assetID).Scan(&exists)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("Record check asset: %w", err)
	}

	now := time.Now().UTC()
	if _, err := s.db.Exec(
		`INSERT INTO asset_views(user_id, asset_id, view_count, last_viewed_at)
		 VALUES(?, ?, 1, ?)
		 ON CONFLICT(user_id, asset_id)
		 DO UPDATE SET view_count = view_count + 1, last_viewed_at = excluded.last_viewed_at`,
		userID, assetID, now,
	); err != nil {
		return fmt.Errorf("Record upsert: %w", err)
	}
	return nil
}

// Count returns the current view count for (userID, assetID); 0 if no row.
func (s *ViewsService) Count(userID, assetID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT view_count FROM asset_views WHERE user_id=? AND asset_id=?`,
		userID, assetID,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("Count: %w", err)
	}
	return n, nil
}
