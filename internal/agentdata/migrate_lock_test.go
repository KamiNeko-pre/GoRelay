package agentdata

import (
	"sync"
	"testing"

	"go-lua-crawler/internal/testdb"
)

func TestConcurrentAutoMigrateSerializesAcrossConnections(t *testing.T) {
	db := testdb.Open(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(8)

	const workers = 6
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- NewStore(db).Migrate()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Store.Migrate failed: %v", err)
		}
	}
}
