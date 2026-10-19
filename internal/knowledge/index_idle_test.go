package knowledge

import (
	"context"
	"go-lua-crawler/internal/testdb"
	"testing"
)

func TestIndexerIdleIsNormalButDatabaseFailureIsNotSuppressed(t *testing.T) {
	db := testdb.Open(t)
	store := NewStore(db)
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	indexer := &Indexer{Store: store}
	if err := indexer.Once(context.Background()); err != nil {
		t.Fatal("empty queue reported as failure", err)
	}
	// testdb owns a disposable schema, never a user's project database.
	if err := db.Migrator().DropTable(&IndexWork{}); err != nil {
		t.Fatal(err)
	}
	if err := indexer.Once(context.Background()); err == nil {
		t.Fatal("real database failure hidden as idle")
	}
}
