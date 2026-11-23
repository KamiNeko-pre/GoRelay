package repository

import (
	"bytes"
	"context"
	"errors"
	"gorm.io/gorm"
	"strings"
	"testing"
	"time"
)

func TestDatabaseLoggerDistinguishesExpectedMissingRecordFromFailure(t *testing.T) {
	var output bytes.Buffer
	logger := databaseLogger(&output)
	query := func() (string, int64) { return "SELECT expected_record", 0 }
	logger.Trace(context.Background(), time.Now(), query, gorm.ErrRecordNotFound)
	if output.Len() != 0 {
		t.Fatal("expected absence printed as error")
	}
	logger.Trace(context.Background(), time.Now(), query, errors.New("fixture database unavailable"))
	if !strings.Contains(output.String(), "fixture database unavailable") {
		t.Fatal("real database failure suppressed")
	}
}
