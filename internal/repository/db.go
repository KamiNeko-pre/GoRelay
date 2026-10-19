package repository

import (
	"errors"
	"fmt"
	mysqldriver "github.com/go-sql-driver/mysql"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/logger"
	taskmodel "go-lua-crawler/internal/task"
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

func InitDB() error {
	// 数据库配置在启动时读取一次。GORM 连接成功后由任务、分析和执行链路共享使用。
	cfg := config.Get()

	dsn := cfg.MySQL.DSN

	// 空白 DSN 代表数据库配置缺失，应在启动阶段直接失败，
	// 而不是构造无效的 MySQL 连接参数并把问题拖到后续连接阶段。
	if err := validateMySQLDSN(dsn); err != nil {
		return fmt.Errorf("validate MySQL DSN: %w", err)
	}
	// Transaction Commit has no context parameter in database/sql. Bound the
	// transport as well, so a lost COMMIT reply cannot block shutdown forever.
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

	// AutoMigrate 会创建缺失的表和索引，但不会执行破坏性删除，
	// 适合开发阶段保持模型与表结构同步。
	if err := db.AutoMigrate(&taskmodel.CrawlTask{}, &taskmodel.TaskOutbox{}, &taskmodel.TaskEvent{}, &CrawlResult{}); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}
	DB = db
	if err := agentdata.NewStore(db).Migrate(); err != nil {
		return fmt.Errorf("migrate Agent facts: %w", err)
	}
	if err := knowledge.NewStore(db).Migrate(); err != nil {
		return fmt.Errorf("migrate knowledge index: %w", err)
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
