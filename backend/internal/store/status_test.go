package store

import (
	"fmt"
	"sync"
	"testing"
)

// setupRecordFixture creates a seed and a target and returns their ids so the
// relay_records assertions below can focus on the record semantics.
func setupRecordFixture(t *testing.T, repo *Repo) (seedID, targetID int64) {
	t.Helper()
	sd := &Seed{SourceSite: "site", InfoHash: "h", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}
	tgt := &Target{Name: "t", Type: "nexusphp", Version: "api", Status: "active"}
	if err := repo.UpsertTarget(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	return sd.ID, tgt.ID
}

func TestUpsertRecordConcurrentClaim(t *testing.T) {
	repo, _ := newTestRepo(t)
	seedID, targetID := setupRecordFixture(t, repo)

	const n = 2
	type result struct {
		inserted bool
		err      error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := &RelayRecord{SeedID: seedID, TargetID: targetID, Role: "publisher", Status: "pending"}
			inserted, err := repo.UpsertRecord(ctx, rec)
			results <- result{inserted: inserted, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var winners int
	for res := range results {
		if res.err != nil {
			t.Fatalf("UpsertRecord: %v", res.err)
		}
		if res.inserted {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one goroutine must win the claim, got %d", winners)
	}
	if got := rawCount(t, repo, `SELECT count(*) FROM relay_records WHERE seed_id=? AND target_id=?`, seedID, targetID); got != 1 {
		t.Fatalf("relay row count = %d, want 1", got)
	}
}

func TestUpdateRecordAttempt(t *testing.T) {
	repo, _ := newTestRepo(t)
	seedID, targetID := setupRecordFixture(t, repo)
	if inserted, err := repo.UpsertRecord(ctx, &RelayRecord{SeedID: seedID, TargetID: targetID, Role: "publisher", Status: "pending"}); err != nil || !inserted {
		t.Fatalf("UpsertRecord = (%v, %v), want (true, nil)", inserted, err)
	}

	if err := repo.UpdateRecordAttempt(ctx, seedID, targetID, "failed", "boom"); err != nil {
		t.Fatalf("UpdateRecordAttempt: %v", err)
	}
	got, err := repo.GetRecord(ctx, seedID, targetID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got.Attempts != 1 || got.Status != "failed" || got.LastError != "boom" {
		t.Fatalf("attempt 1 mismatch: %+v", got)
	}

	if err := repo.UpdateRecordAttempt(ctx, seedID, targetID, "uploading", ""); err != nil {
		t.Fatalf("UpdateRecordAttempt 2: %v", err)
	}
	got, err = repo.GetRecord(ctx, seedID, targetID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got.Attempts != 2 || got.Status != "uploading" || got.LastError != "" {
		t.Fatalf("attempt 2 mismatch: %+v", got)
	}
}

func TestUpdateRecordStatusDoesNotTouchRetired(t *testing.T) {
	repo, _ := newTestRepo(t)
	seedID, targetID := setupRecordFixture(t, repo)
	if inserted, err := repo.UpsertRecord(ctx, &RelayRecord{SeedID: seedID, TargetID: targetID, Role: "publisher", Status: "pending"}); err != nil || !inserted {
		t.Fatalf("UpsertRecord = (%v, %v), want (true, nil)", inserted, err)
	}

	if err := repo.MarkRetired(ctx, seedID, targetID, "seeders >= 10"); err != nil {
		t.Fatalf("MarkRetired: %v", err)
	}
	before, err := repo.GetRecord(ctx, seedID, targetID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if before.RetiredAt == 0 || before.RetireReason != "seeders >= 10" {
		t.Fatalf("MarkRetired not persisted: %+v", before)
	}

	// UpdateRecordStatus must leave retired_at / retire_reason untouched.
	if err := repo.UpdateRecordStatus(ctx, seedID, targetID, "retired", ""); err != nil {
		t.Fatalf("UpdateRecordStatus: %v", err)
	}
	after, err := repo.GetRecord(ctx, seedID, targetID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if after.RetiredAt != before.RetiredAt || after.RetireReason != before.RetireReason {
		t.Fatalf("retired_at/retire_reason overwritten: before=%+v after=%+v", before, after)
	}
}

func TestSetRecordRole(t *testing.T) {
	repo, _ := newTestRepo(t)
	seedID, targetID := setupRecordFixture(t, repo)
	if inserted, err := repo.UpsertRecord(ctx, &RelayRecord{SeedID: seedID, TargetID: targetID, Role: "publisher", Status: "pending"}); err != nil || !inserted {
		t.Fatalf("UpsertRecord = (%v, %v), want (true, nil)", inserted, err)
	}

	if err := repo.SetRecordRole(ctx, seedID, targetID, "seeder"); err != nil {
		t.Fatalf("SetRecordRole: %v", err)
	}
	got, err := repo.GetRecord(ctx, seedID, targetID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got.Role != "seeder" || got.Status != "pending" || got.Attempts != 0 {
		t.Fatalf("SetRecordRole touched non-role columns: %+v", got)
	}
}

func TestAppendLogSeed(t *testing.T) {
	repo, _ := newTestRepo(t)
	sd := &Seed{SourceSite: "site", InfoHash: "h", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendLogSeed(ctx, sd.ID, "info", "relay_started", "detail here"); err != nil {
		t.Fatalf("AppendLogSeed: %v", err)
	}

	var seedID int64
	if err := repo.DB().QueryRow(
		`SELECT seed_id FROM activity_log WHERE action='relay_started'`).Scan(&seedID); err != nil {
		t.Fatalf("read seed_id: %v", err)
	}
	if seedID != sd.ID {
		t.Fatalf("activity_log.seed_id = %d, want %d", seedID, sd.ID)
	}
}

func TestSeedStatusWhitelist(t *testing.T) {
	repo, _ := newTestRepo(t)
	valid := []string{"discovered", "downloading", "downloaded", "processing", "seeding", "retry", "failed", "retired", "skipped"}
	for i, s := range valid {
		sd := &Seed{SourceSite: fmt.Sprintf("site-%d", i), InfoHash: fmt.Sprintf("hash-%d", i), Status: s}
		if _, err := repo.CreateSeed(ctx, sd); err != nil {
			t.Fatalf("valid seed status %q rejected: %v", s, err)
		}
	}

	if _, err := repo.CreateSeed(ctx, &Seed{SourceSite: "bad", InfoHash: "bad", Status: "bogus"}); err == nil {
		t.Fatal("expected invalid seed status to be rejected by CreateSeed")
	}

	sd := &Seed{SourceSite: "upd", InfoHash: "upd", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateSeedStatus(ctx, sd.ID, "bogus", ""); err == nil {
		t.Fatal("expected invalid seed status to be rejected by UpdateSeedStatus")
	}
}

func TestRecordStatusWhitelist(t *testing.T) {
	repo, _ := newTestRepo(t)
	sd := &Seed{SourceSite: "site", InfoHash: "h", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	valid := []string{"pending", "uploading", "published", "cross_seeding", "seeding", "failed", "retired", "skipped_existing"}
	var first *RelayRecord
	for i, s := range valid {
		tgt := &Target{Name: fmt.Sprintf("t-%d", i), Type: "nexusphp", Version: "api", Status: "active"}
		if err := repo.UpsertTarget(ctx, tgt); err != nil {
			t.Fatal(err)
		}
		rec := &RelayRecord{SeedID: sd.ID, TargetID: tgt.ID, Role: "publisher", Status: s}
		inserted, err := repo.UpsertRecord(ctx, rec)
		if err != nil {
			t.Fatalf("valid record status %q rejected: %v", s, err)
		}
		if !inserted {
			t.Fatalf("valid record status %q: expected insert", s)
		}
		if i == 0 {
			first = rec
		}
	}

	if _, err := repo.UpsertRecord(ctx, &RelayRecord{SeedID: sd.ID, TargetID: 99999, Status: "bogus"}); err == nil {
		t.Fatal("expected invalid record status to be rejected by UpsertRecord")
	}
	if err := repo.UpdateRecordStatus(ctx, sd.ID, first.TargetID, "bogus", ""); err == nil {
		t.Fatal("expected invalid record status to be rejected by UpdateRecordStatus")
	}
	if err := repo.UpdateRecordAttempt(ctx, sd.ID, first.TargetID, "bogus", ""); err == nil {
		t.Fatal("expected invalid record status to be rejected by UpdateRecordAttempt")
	}
}
