package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func newTestRepo(t *testing.T) (*Repo, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewRepo(st.DB(), testKey()), st
}

func rawString(t *testing.T, repo *Repo, query string, args ...any) string {
	t.Helper()
	var s string
	if err := repo.DB().QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("raw query %q: %v", query, err)
	}
	return s
}

func rawCount(t *testing.T, repo *Repo, query string, args ...any) int {
	t.Helper()
	var n int
	if err := repo.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("raw count %q: %v", query, err)
	}
	return n
}

func TestSourceCRUDRoundtrip(t *testing.T) {
	repo, _ := newTestRepo(t)

	src := &Source{
		Name: "src1", Role: "source", BaseURL: "http://src", RSSURL: "http://src/rss",
		AnnounceURL: "http://src/ann", Status: "active",
	}
	if err := repo.UpsertSource(ctx, src); err != nil {
		t.Fatalf("UpsertSource insert: %v", err)
	}
	if src.ID == 0 {
		t.Fatal("expected UpsertSource to assign an id")
	}

	got, err := repo.GetSourceByID(ctx, src.ID)
	if err != nil {
		t.Fatalf("GetSourceByID: %v", err)
	}
	if got.Name != "src1" || got.Role != "source" || got.Status != "active" ||
		got.BaseURL != "http://src" || got.RSSURL != "http://src/rss" || got.AnnounceURL != "http://src/ann" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Fatalf("expected timestamps to be set: %+v", got)
	}

	// Update path.
	src.Name = "src1-renamed"
	src.FailCount = 2
	if err := repo.UpsertSource(ctx, src); err != nil {
		t.Fatalf("UpsertSource update: %v", err)
	}
	got, err = repo.GetSourceByID(ctx, src.ID)
	if err != nil {
		t.Fatalf("GetSourceByID after update: %v", err)
	}
	if got.Name != "src1-renamed" || got.FailCount != 2 {
		t.Fatalf("update not persisted: %+v", got)
	}

	// SetSourceStatus.
	if err := repo.SetSourceStatus(ctx, src.ID, "paused"); err != nil {
		t.Fatalf("SetSourceStatus: %v", err)
	}
	got, _ = repo.GetSourceByID(ctx, src.ID)
	if got.Status != "paused" {
		t.Fatalf("status = %q, want paused", got.Status)
	}

	// IncSourceFail returns the incremented value.
	if n, err := repo.IncSourceFail(ctx, src.ID); err != nil || n != 3 {
		t.Fatalf("IncSourceFail = (%d, %v), want (3, nil)", n, err)
	}
	if n, err := repo.IncSourceFail(ctx, src.ID); err != nil || n != 4 {
		t.Fatalf("IncSourceFail = (%d, %v), want (4, nil)", n, err)
	}
}

func TestSourcePauseWritesLog(t *testing.T) {
	repo, _ := newTestRepo(t)
	src := &Source{Name: "src", Role: "source", Status: "active"}
	if err := repo.UpsertSource(ctx, src); err != nil {
		t.Fatalf("UpsertSource: %v", err)
	}
	if err := repo.PauseSource(ctx, src.ID, "flaky rss"); err != nil {
		t.Fatalf("PauseSource: %v", err)
	}
	got, _ := repo.GetSourceByID(ctx, src.ID)
	if got.Status != "paused" {
		t.Fatalf("status = %q, want paused", got.Status)
	}
	if n := rawCount(t, repo, `SELECT count(*) FROM activity_log WHERE action='source_paused' AND detail='flaky rss'`); n != 1 {
		t.Fatalf("activity log count = %d, want 1", n)
	}
}

func TestSourceGetActiveFilters(t *testing.T) {
	repo, _ := newTestRepo(t)
	if err := repo.UpsertSource(ctx, &Source{Name: "a", Role: "source", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSource(ctx, &Source{Name: "b", Role: "source", Status: "paused"}); err != nil {
		t.Fatal(err)
	}
	active, err := repo.GetActiveSources(ctx)
	if err != nil {
		t.Fatalf("GetActiveSources: %v", err)
	}
	if len(active) != 1 || active[0].Name != "a" {
		t.Fatalf("active sources = %+v, want only 'a'", active)
	}
}

func TestCredentialRoundtripEncryptedAtRest(t *testing.T) {
	repo, _ := newTestRepo(t)
	src := &Source{
		Name: "cred", Role: "source", Status: "active",
		Cookie: "uid=123; pass=abc", Passkey: "supersecret", APIToken: "tok-xyz",
	}
	if err := repo.UpsertSource(ctx, src); err != nil {
		t.Fatalf("UpsertSource: %v", err)
	}

	// Read back: plaintext must match, and must be non-empty.
	got, err := repo.GetSourceByID(ctx, src.ID)
	if err != nil {
		t.Fatalf("GetSourceByID: %v", err)
	}
	if got.Cookie != "uid=123; pass=abc" || got.Passkey != "supersecret" || got.APIToken != "tok-xyz" {
		t.Fatalf("plaintext mismatch: %+v", got)
	}

	// At rest: the enc_* columns must not contain the plaintext.
	enc := rawString(t, repo, `SELECT enc_passkey FROM sources WHERE id = ?`, src.ID)
	if enc == "" || enc == "supersecret" || strings.Contains(enc, "supersecret") {
		t.Fatalf("enc_passkey at rest = %q, want non-plaintext ciphertext", enc)
	}
}

func TestCredentialEmptyStoredAsNull(t *testing.T) {
	repo, _ := newTestRepo(t)
	src := &Source{Name: "empty", Role: "source", Status: "active"}
	if err := repo.UpsertSource(ctx, src); err != nil {
		t.Fatalf("UpsertSource: %v", err)
	}
	var nullable any
	if err := repo.DB().QueryRow(`SELECT enc_passkey FROM sources WHERE id = ?`, src.ID).Scan(&nullable); err != nil {
		t.Fatalf("scan enc_passkey: %v", err)
	}
	if nullable != nil {
		t.Fatalf("enc_passkey = %v, want NULL for empty plaintext", nullable)
	}
	got, err := repo.GetSourceByID(ctx, src.ID)
	if err != nil {
		t.Fatalf("GetSourceByID: %v", err)
	}
	if got.Passkey != "" {
		t.Fatalf("passkey = %q, want empty", got.Passkey)
	}
}

func TestCredentialDecryptFailureReturnsError(t *testing.T) {
	_, st := newTestRepo(t)
	repoA := NewRepo(st.DB(), testKey())
	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(i + 99)
	}
	repoB := NewRepo(st.DB(), other)

	src := &Source{Name: "cred", Role: "source", Status: "active", Passkey: "secret"}
	if err := repoA.UpsertSource(ctx, src); err != nil {
		t.Fatalf("UpsertSource: %v", err)
	}
	if _, err := repoB.GetSourceByID(ctx, src.ID); err == nil {
		t.Fatal("expected a decrypt error when reading with the wrong key")
	}
}

func TestListSkipsUndecryptableRow(t *testing.T) {
	repo, _ := newTestRepo(t)
	if err := repo.UpsertSource(ctx, &Source{Name: "good", Role: "source", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	// A second active row with a corrupted ciphertext: the list must skip it
	// (slog.Warn) instead of failing the whole table.
	if _, err := repo.DB().Exec(
		`INSERT INTO sources (name, role, status, enc_passkey) VALUES ('bad','source','active','!!!not-base64!!!')`); err != nil {
		t.Fatalf("insert bad row: %v", err)
	}

	active, err := repo.GetActiveSources(ctx)
	if err != nil {
		t.Fatalf("GetActiveSources must not fail on one undecryptable row: %v", err)
	}
	if len(active) != 1 || active[0].Name != "good" {
		t.Fatalf("active sources = %+v, want only 'good'", active)
	}
}

func TestTargetCRUDAndEnabledFilter(t *testing.T) {
	repo, _ := newTestRepo(t)
	tgt := &Target{
		Name: "t1", Type: "nexusphp", Version: "api", BaseURL: "http://t1",
		AnnounceURL: "http://t1/ann", TestMode: 1, FallbackCategory: "cat",
		CategoryOverrides: `{"a":1}`, DimensionOverrides: `{"b":2}`,
		TagsMap: `{"国语":"1"}`,
		Cookie:  "c", Passkey: "p", APIToken: "t", Status: "active",
	}
	if err := repo.UpsertTarget(ctx, tgt); err != nil {
		t.Fatalf("UpsertTarget insert: %v", err)
	}
	if tgt.ID == 0 {
		t.Fatal("expected id")
	}
	got, err := repo.GetTargetByID(ctx, tgt.ID)
	if err != nil {
		t.Fatalf("GetTargetByID: %v", err)
	}
	if got.Name != "t1" || got.Type != "nexusphp" || got.Version != "api" ||
		got.TestMode != 1 || got.CategoryOverrides != `{"a":1}` ||
		got.TagsMap != `{"国语":"1"}` ||
		got.Passkey != "p" || got.Status != "active" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// A paused target must be excluded from GetEnabledTargets.
	if err := repo.UpsertTarget(ctx, &Target{Name: "t2", Type: "mteam", Version: "api", Status: "paused"}); err != nil {
		t.Fatal(err)
	}
	enabled, err := repo.GetEnabledTargets(ctx)
	if err != nil {
		t.Fatalf("GetEnabledTargets: %v", err)
	}
	if len(enabled) != 1 || enabled[0].Name != "t1" {
		t.Fatalf("enabled targets = %+v, want only t1", enabled)
	}
}

func TestQBInstanceCRUDAndTouch(t *testing.T) {
	repo, _ := newTestRepo(t)
	q := &QBInstance{Name: "qb1", Host: "localhost", Port: 8080, Username: "admin", Password: "pw", Priority: 5, Enabled: 1}
	if err := repo.UpsertQBInstance(ctx, q); err != nil {
		t.Fatalf("UpsertQBInstance insert: %v", err)
	}
	if q.ID == 0 {
		t.Fatal("expected id")
	}
	enabled, err := repo.GetEnabledQBInstances(ctx)
	if err != nil {
		t.Fatalf("GetEnabledQBInstances: %v", err)
	}
	if len(enabled) != 1 || enabled[0].Password != "pw" || enabled[0].LastSeenAt != 0 {
		t.Fatalf("qb roundtrip mismatch: %+v", enabled)
	}
	if err := repo.TouchQBSeen(ctx, q.ID); err != nil {
		t.Fatalf("TouchQBSeen: %v", err)
	}
	enabled, _ = repo.GetEnabledQBInstances(ctx)
	if enabled[0].LastSeenAt == 0 {
		t.Fatal("expected LastSeenAt to be stamped")
	}
}

func TestSeedCRUDAndIdempotentCreate(t *testing.T) {
	repo, _ := newTestRepo(t)
	sd := &Seed{
		SourceSite: "site-a", InfoHash: "hash-1", Title: "Movie", Size: 1234,
		Category: "movie", Promotion: "free", Status: "discovered",
	}
	id1, err := repo.CreateSeed(ctx, sd)
	if err != nil {
		t.Fatalf("CreateSeed: %v", err)
	}
	if id1 == 0 || sd.ID != id1 {
		t.Fatalf("CreateSeed id = %d, sd.ID = %d", id1, sd.ID)
	}

	// Idempotent: same (source_site, info_hash) returns the existing id, no new row.
	dup := &Seed{SourceSite: "site-a", InfoHash: "hash-1", Title: "ignored", Status: "discovered"}
	id2, err := repo.CreateSeed(ctx, dup)
	if err != nil {
		t.Fatalf("CreateSeed dup: %v", err)
	}
	if id2 != id1 || dup.ID != id1 {
		t.Fatalf("CreateSeed dup id = %d, want %d", id2, id1)
	}
	if n := rawCount(t, repo, `SELECT count(*) FROM seeds WHERE source_site='site-a' AND info_hash='hash-1'`); n != 1 {
		t.Fatalf("seed row count = %d, want 1", n)
	}

	got, err := repo.GetSeedByHash(ctx, "site-a", "hash-1")
	if err != nil {
		t.Fatalf("GetSeedByHash: %v", err)
	}
	if got.Title != "Movie" || got.Size != 1234 || got.Category != "movie" || got.Promotion != "free" {
		t.Fatalf("seed roundtrip mismatch: %+v", got)
	}
	if got.DiscoveredAt == 0 {
		t.Fatal("expected discovered_at to be set")
	}

	if err := repo.UpdateSeedStatus(ctx, id1, "downloading", ""); err != nil {
		t.Fatalf("UpdateSeedStatus: %v", err)
	}
	if err := repo.BumpRetry(ctx, id1); err != nil {
		t.Fatalf("BumpRetry: %v", err)
	}
	got, _ = repo.GetSeedByHash(ctx, "site-a", "hash-1")
	if got.Status != "downloading" || got.RetryCount != 1 {
		t.Fatalf("seed status/retry mismatch: %+v", got)
	}

	list, err := repo.ListSeedsByStatus(ctx, "downloading")
	if err != nil || len(list) != 1 || list[0].ID != id1 {
		t.Fatalf("ListSeedsByStatus = %+v, err=%v", list, err)
	}
	recent, err := repo.ListRecentSeeds(ctx, 10)
	if err != nil || len(recent) != 1 {
		t.Fatalf("ListRecentSeeds = %+v, err=%v", recent, err)
	}
}

func TestUpdateSeedPromotion(t *testing.T) {
	repo, _ := newTestRepo(t)
	sd := &Seed{SourceSite: "site", InfoHash: "hash", Title: "Movie", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}

	if err := repo.UpdateSeedPromotion(ctx, sd.ID, "2x_free"); err != nil {
		t.Fatalf("UpdateSeedPromotion: %v", err)
	}
	got, err := repo.GetSeedByID(ctx, sd.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Promotion != "2x_free" {
		t.Fatalf("promotion = %q, want 2x_free", got.Promotion)
	}

	// A later fetch may refine the marker; overwrite must be allowed.
	if err := repo.UpdateSeedPromotion(ctx, sd.ID, "free"); err != nil {
		t.Fatalf("UpdateSeedPromotion overwrite: %v", err)
	}
	got, _ = repo.GetSeedByID(ctx, sd.ID)
	if got.Promotion != "free" {
		t.Fatalf("promotion after overwrite = %q, want free", got.Promotion)
	}
}

func TestRelayRecordCRUD(t *testing.T) {
	repo, _ := newTestRepo(t)
	src := &Source{Name: "s", Role: "source", Status: "active"}
	if err := repo.UpsertSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	sd := &Seed{SourceSite: "site", InfoHash: "h", Status: "discovered"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}
	tgt := &Target{Name: "t", Type: "nexusphp", Version: "api", Status: "active"}
	if err := repo.UpsertTarget(ctx, tgt); err != nil {
		t.Fatal(err)
	}

	rec := &RelayRecord{SeedID: sd.ID, TargetID: tgt.ID, Role: "publisher", Status: "pending"}
	inserted, err := repo.UpsertRecord(ctx, rec)
	if err != nil {
		t.Fatalf("UpsertRecord: %v", err)
	}
	if !inserted {
		t.Fatal("expected first UpsertRecord to insert")
	}
	if rec.ID == 0 {
		t.Fatal("expected record id")
	}
	got, err := repo.GetRecord(ctx, sd.ID, tgt.ID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got.Role != "publisher" || got.Status != "pending" || got.Attempts != 0 {
		t.Fatalf("record roundtrip mismatch: %+v", got)
	}

	if err := repo.UpdateRecordStatus(ctx, sd.ID, tgt.ID, "uploading", "boom"); err != nil {
		t.Fatalf("UpdateRecordStatus: %v", err)
	}
	got, _ = repo.GetRecord(ctx, sd.ID, tgt.ID)
	if got.Status != "uploading" || got.LastError != "boom" {
		t.Fatalf("status/error mismatch: %+v", got)
	}

	if err := repo.MarkRetired(ctx, sd.ID, tgt.ID, "seeders >= 10"); err != nil {
		t.Fatalf("MarkRetired: %v", err)
	}
	got, _ = repo.GetRecord(ctx, sd.ID, tgt.ID)
	if got.RetiredAt == 0 || got.RetireReason != "seeders >= 10" {
		t.Fatalf("retire mismatch: %+v", got)
	}

	// Upsert on the same (seed, target) is now an atomic no-op claim: the row
	// is already claimed, so inserted=false and no duplicate is created.
	inserted, err = repo.UpsertRecord(ctx, rec)
	if err != nil {
		t.Fatalf("UpsertRecord again: %v", err)
	}
	if inserted {
		t.Fatal("expected second UpsertRecord to report inserted=false on conflict")
	}
	if n := rawCount(t, repo, `SELECT count(*) FROM relay_records WHERE seed_id=? AND target_id=?`, sd.ID, tgt.ID); n != 1 {
		t.Fatalf("relay row count = %d, want 1", n)
	}

	recs, err := repo.ListRecordsBySeed(ctx, sd.ID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("ListRecordsBySeed = %+v, err=%v", recs, err)
	}
}

func TestReplicaCRUD(t *testing.T) {
	repo, _ := newTestRepo(t)
	sd := &Seed{SourceSite: "site", InfoHash: "h", Status: "seeding"}
	if _, err := repo.CreateSeed(ctx, sd); err != nil {
		t.Fatal(err)
	}
	q := &QBInstance{Name: "qb", Host: "h", Port: 1, Username: "u", Enabled: 1}
	if err := repo.UpsertQBInstance(ctx, q); err != nil {
		t.Fatal(err)
	}

	rep := &Replica{SeedID: sd.ID, QBID: q.ID, InfoHash: "h", Role: "origin", Status: "pending", Progress: 0}
	if err := repo.UpsertReplica(ctx, rep); err != nil {
		t.Fatalf("UpsertReplica: %v", err)
	}
	if rep.ID == 0 {
		t.Fatal("expected replica id")
	}
	if err := repo.UpsertReplica(ctx, rep); err != nil {
		t.Fatalf("UpsertReplica again: %v", err)
	}
	if n := rawCount(t, repo, `SELECT count(*) FROM seed_replicas WHERE seed_id=? AND qb_id=?`, sd.ID, q.ID); n != 1 {
		t.Fatalf("replica row count = %d, want 1", n)
	}

	if err := repo.UpdateReplicaProgress(ctx, rep.ID, 0.75); err != nil {
		t.Fatalf("UpdateReplicaProgress: %v", err)
	}
	reps, err := repo.ListReplicas(ctx, sd.ID)
	if err != nil || len(reps) != 1 || reps[0].Progress != 0.75 {
		t.Fatalf("ListReplicas = %+v, err=%v", reps, err)
	}

	if err := repo.DeleteReplica(ctx, rep.ID); err != nil {
		t.Fatalf("DeleteReplica: %v", err)
	}
	reps, _ = repo.ListReplicas(ctx, sd.ID)
	if len(reps) != 0 {
		t.Fatalf("replicas after delete = %+v, want empty", reps)
	}
}

func TestNotifierAndRoutes(t *testing.T) {
	repo, _ := newTestRepo(t)
	n := &NotifierInstance{Type: "telegram", Name: "tg", Config: `{"token":"abc"}`, Enabled: 1}
	if err := repo.UpsertNotifierInstance(ctx, n); err != nil {
		t.Fatalf("UpsertNotifierInstance: %v", err)
	}
	if n.ID == 0 {
		t.Fatal("expected instance id")
	}
	// Config must be encrypted at rest.
	enc := rawString(t, repo, `SELECT enc_config FROM notifier_instances WHERE id = ?`, n.ID)
	if enc == "" || strings.Contains(enc, "abc") {
		t.Fatalf("enc_config at rest = %q, want ciphertext", enc)
	}
	off := &NotifierInstance{Type: "webhook", Name: "wh", Config: `{"url":"x"}`, Enabled: 0}
	if err := repo.UpsertNotifierInstance(ctx, off); err != nil {
		t.Fatal(err)
	}

	all, err := repo.GetNotifierInstances(ctx, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("GetNotifierInstances(false) = %+v, err=%v", all, err)
	}
	enabled, err := repo.GetNotifierInstances(ctx, true)
	if err != nil || len(enabled) != 1 || enabled[0].ID != n.ID || enabled[0].Config != `{"token":"abc"}` {
		t.Fatalf("GetNotifierInstances(true) = %+v, err=%v", enabled, err)
	}

	// Routes: two instances × multiple tiers.
	for _, rt := range []*Route{
		{InstanceID: n.ID, Tier: "critical", Enabled: 1},
		{InstanceID: n.ID, Tier: "info", Enabled: 1},
		{InstanceID: off.ID, Tier: "critical", Enabled: 0},
		{InstanceID: off.ID, Tier: "warning", Enabled: 1},
	} {
		if err := repo.UpsertNotifierRoute(ctx, rt); err != nil {
			t.Fatalf("UpsertNotifierRoute: %v", err)
		}
	}
	crit, err := repo.GetRoutes(ctx, "critical")
	if err != nil {
		t.Fatalf("GetRoutes: %v", err)
	}
	if len(crit) != 2 {
		t.Fatalf("critical routes = %+v, want 2", crit)
	}
	// Upsert the same (instance, tier) must overwrite, not duplicate.
	if err := repo.UpsertNotifierRoute(ctx, &Route{InstanceID: n.ID, Tier: "critical", Enabled: 0}); err != nil {
		t.Fatal(err)
	}
	crit, _ = repo.GetRoutes(ctx, "critical")
	if len(crit) != 2 {
		t.Fatalf("critical routes after upsert = %+v, want 2 (no dup)", crit)
	}
	for _, rt := range crit {
		if rt.InstanceID == n.ID && rt.Enabled != 0 {
			t.Fatalf("expected n.ID critical route disabled after upsert: %+v", rt)
		}
	}
}

func TestStrategyCRUD(t *testing.T) {
	repo, _ := newTestRepo(t)
	st, err := repo.GetStrategy(ctx)
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	if st.ID != 1 || st.RetireSeeders != 10 || st.RetireMinutes != 60 || st.RetireMode != "and" || st.DispatchMode != "priority" || st.RetryMax != 3 {
		t.Fatalf("default strategy mismatch: %+v", st)
	}

	upd := &Strategy{
		ID: 1, Promotions: `["free"]`, Keywords: `["x264"]`, MinSize: 100, MaxSize: 200,
		RetireSeeders: 5, RetireMinutes: 30, RetireRatioEnabled: 1, RetireRatio: 2.5,
		RetireMode: "or", DispatchMode: "least_jobs", Timezone: "Asia/Shanghai",
		ImageHost: `{"url":"https://img"}`, ImageCoverEnabled: 1, RetryMax: 5,
	}
	if err := repo.UpdateStrategy(ctx, upd); err != nil {
		t.Fatalf("UpdateStrategy: %v", err)
	}
	got, err := repo.GetStrategy(ctx)
	if err != nil {
		t.Fatalf("GetStrategy after update: %v", err)
	}
	if got.Promotions != `["free"]` || got.Keywords != `["x264"]` || got.MinSize != 100 ||
		got.MaxSize != 200 || got.RetireRatio != 2.5 || got.RetireMode != "or" ||
		got.DispatchMode != "least_jobs" || got.Timezone != "Asia/Shanghai" ||
		got.ImageCoverEnabled != 1 || got.RetryMax != 5 {
		t.Fatalf("strategy update mismatch: %+v", got)
	}
}

func TestGetByIDNotFound(t *testing.T) {
	repo, _ := newTestRepo(t)
	if _, err := repo.GetSourceByID(ctx, 999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSourceByID missing: err = %v, want sql.ErrNoRows", err)
	}
	if _, err := repo.GetSeedByHash(ctx, "nope", "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSeedByHash missing: err = %v, want sql.ErrNoRows", err)
	}
}
