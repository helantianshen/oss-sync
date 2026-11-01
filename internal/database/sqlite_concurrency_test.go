package database

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/deviceauth"
	"github.com/helantianshen/oss-sync/internal/models"
)

func openConcurrencyDB(t *testing.T, legacy bool) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "concurrency.db")
	var db *gorm.DB
	var err error
	if legacy {
		db, err = gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	} else {
		db, err = Init(&config.Config{Database: config.DatabaseConfig{Driver: "sqlite", DSN: path}})
	}
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
}

func TestSQLiteLegacyReadUpgradeReproducesBusy(t *testing.T) {
	db := openConcurrencyDB(t, true)
	if err := db.Exec("CREATE TABLE counter (value INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO counter VALUES (0)").Error; err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback()
	b, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Rollback()
	for _, tx := range []*sql.Tx{a, b} {
		var value int
		if err := tx.QueryRowContext(ctx, "SELECT value FROM counter").Scan(&value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.ExecContext(ctx, "UPDATE counter SET value = value + 1"); err != nil {
		t.Fatal(err)
	}
	_, err = b.ExecContext(ctx, "UPDATE counter SET value = value + 1")
	if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Fatalf("expected read-to-write lock conflict, got %v", err)
	}
	t.Log("legacy deferred read-to-write transaction reproduced SQLITE_BUSY")
}

func TestSQLiteReservesWriteIntentAtTransactionStart(t *testing.T) {
	db := openConcurrencyDB(t, false)
	pool, _ := db.DB()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback()
	b, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.ExecContext(ctx, "PRAGMA busy_timeout=25"); err != nil {
		t.Fatal(err)
	}
	_, err = b.ExecContext(ctx, "BEGIN IMMEDIATE")
	if err == nil {
		_, _ = b.ExecContext(ctx, "ROLLBACK")
		t.Fatal("a transaction did not reserve write intent before its first query")
	}
	if !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Fatal(err)
	}
	if err := a.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	if _, err := b.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteConcurrentDeviceActivityKeepsCursor(t *testing.T) {
	db := openConcurrencyDB(t, false)
	if err := db.AutoMigrate(&models.ClientDevice{}, &models.DeviceVault{}); err != nil {
		t.Fatal(err)
	}
	if err := deviceauth.Touch(db, 1, "vault", "device", "test", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	const workers, rounds = 6, 20
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := make(chan struct{})
	failures := make(chan error, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			for round := 0; round < rounds; round++ {
				cursor := int64(worker*rounds + round + 1)
				if err := deviceauth.Touch(db.WithContext(ctx), 1, "vault", "device", "test", &cursor, time.Now()); err != nil {
					failures <- fmt.Errorf("worker %d round %d: %w", worker, round, err)
					return
				}
			}
		}(worker)
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var binding models.DeviceVault
	if err := db.First(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.LastCursor != workers*rounds {
		t.Fatalf("cursor regressed: %d", binding.LastCursor)
	}
	var count int64
	if err := db.Model(&models.DeviceVault{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate device binding: %d", count)
	}
	t.Logf("%d concurrent device activity writes passed; cursor stayed monotonic", workers*rounds)
}

func TestSQLiteConnectionStringPreservesExplicitOptions(t *testing.T) {
	for _, input := range []string{"data/oss.db", ":memory:", "file:memdb?mode=memory&cache=shared", "data/oss.db?_pragma=busy_timeout%281000%29"} {
		got, err := sqliteConnectionString(input)
		if err != nil || !strings.Contains(got, "_txlock=immediate") {
			t.Fatalf("%q: %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"data/oss.db?_txlock=deferred&_pragma=busy_timeout%281000%29", "data/oss.db?_txlock="} {
		if got, err := sqliteConnectionString(input); err != nil || got != input {
			t.Fatalf("explicit option changed: %q, %v", got, err)
		}
	}
	if _, err := sqliteConnectionString("data/oss.db?bad=%zz"); err == nil {
		t.Fatal("malformed query accepted")
	}
}
