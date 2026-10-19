// Package testdb isolates integration tests in freshly created MySQL schemas.
package testdb

import (
	"crypto/sha256"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"os"
	"testing"
	"time"
)

func Open(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("MYSQL_INTEGRATION_DSN")
	if dsn == "" {
		t.Skip("MySQL integration: MYSQL_INTEGRATION_DSN not configured")
	}
	c, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	c.DBName = ""
	c.ParseTime = true
	admin, err := gorm.Open(mysql.Open(c.FormatDSN()), &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	stamp := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", t.Name(), time.Now().UnixNano())))
	name := fmt.Sprintf("gorelay_test_%x", stamp[:10])
	if err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4").Error; err != nil {
		t.Fatal(err)
	}
	c.DBName = name
	db, err := gorm.Open(mysql.Open(c.FormatDSN()), &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sql, _ := db.DB()
		sql.Close()
		admin.Exec("DROP DATABASE `" + name + "`")
		sql, _ = admin.DB()
		sql.Close()
	})
	return db
}
