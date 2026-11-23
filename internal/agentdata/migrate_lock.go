package agentdata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gorm.io/gorm"
)

// AutoMigrate serializes GORM schema migrations per MySQL schema using a
// session-pinned advisory lock so multiple service processes can start concurrently
// against an empty database without triggering DDL races.
func AutoMigrate(db *gorm.DB, models ...any) error {
	if db == nil {
		return fmt.Errorf("migration database unavailable")
	}
	if db.Dialector == nil || db.Dialector.Name() != "mysql" {
		return db.AutoMigrate(models...)
	}
	if db.Statement != nil {
		if _, isTx := db.Statement.ConnPool.(gorm.TxCommitter); isTx {
			return db.AutoMigrate(models...)
		}
	}
	return db.Connection(func(tx *gorm.DB) error {
		var dbName string
		if err := tx.Raw("SELECT COALESCE(DATABASE(), '')").Scan(&dbName).Error; err != nil {
			return fmt.Errorf("identify migration schema: %w", err)
		}
		sum := sha256.Sum256([]byte(dbName))
		lockName := "gorelay_migrate_" + hex.EncodeToString(sum[:16])
		var acquired int
		if err := tx.Raw("SELECT COALESCE(GET_LOCK(?, 60), 0)", lockName).Scan(&acquired).Error; err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		if acquired != 1 {
			return fmt.Errorf("timed out waiting for migration lock on schema %q", dbName)
		}
		defer func() {
			_ = tx.Exec("SELECT RELEASE_LOCK(?)", lockName).Error
		}()
		return tx.AutoMigrate(models...)
	})
}
