package repository

import (
	"errors"
	"fmt"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorelay/internal/config"
	"gorelay/internal/logger"
	taskmodel "gorelay/internal/task"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

var DB *gorm.DB

// ExtraMigrator registers additional module models for AutoMigrate at DB startup.
var ExtraMigrator func(*gorm.DB) error

func InitDB() error {
	cfg := config.Get()
	dsn := cfg.MySQL.DSN

	if err := validateMySQLDSN(dsn); err != nil {
		return fmt.Errorf("validate MySQL DSN: %w", err)
	}
	parsedDSN, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parse MySQL DSN: %w", err)
	}
	if parsedDSN.Timeout == 0 {
		parsedDSN.Timeout = 3 * time.Second
	}
	if parsedDSN.ReadTimeout == 0 {
		parsedDSN.ReadTimeout = 10 * time.Second
	}
	if parsedDSN.WriteTimeout == 0 {
		parsedDSN.WriteTimeout = 10 * time.Second
	}
	dsn = parsedDSN.FormatDSN()
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{TranslateError: true, Logger: databaseLogger(os.Stderr)})
	if err != nil {
		return fmt.Errorf("open MySQL connection: %w", err)
	}
	logger.Log.Info("数据库连接成功", zap.String("type", "mysql"))

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get MySQL connection pool: %w", err)
	}
	sqlDB.SetMaxIdleConns(cfg.MySQL.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.MySQL.MaxOpenConns)

	if err := AutoMigrate(db, &taskmodel.CrawlTask{}, &taskmodel.TaskOutbox{}, &taskmodel.TaskEvent{}, &CrawlResult{}, &FrozenRule{}); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}
	DB = db
	if ExtraMigrator != nil {
		if err := ExtraMigrator(db); err != nil {
			return fmt.Errorf("migrate extra models: %w", err)
		}
	}
	logger.Log.Info("数据库表结构同步完成")
	return nil
}
func validateMySQLDSN(dsn string) error {
	if strings.TrimSpace(dsn) == "" {
		return errors.New("mysql.dsn is required")
	}
	return nil
}
func databaseLogger(output io.Writer) gormlogger.Interface {
	return gormlogger.New(log.New(output, "", log.LstdFlags), gormlogger.Config{SlowThreshold: 200 * time.Millisecond, LogLevel: gormlogger.Warn, IgnoreRecordNotFoundError: true, ParameterizedQueries: true})
}
