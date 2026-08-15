package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// ---- types ----

// Promotion values for seeds.promotion.
const (
	PromoFree       = "free"
	Promo2xFree     = "2xfree"
	PromoHalfOff    = "half_off"
	PromoThirtyPct  = "thirty_pct"
	PromoNormal     = "normal"
)

// Seed status values for seeds.status.
const (
	SeedStatusDiscovered   = "discovered"
	SeedStatusDownloading  = "downloading"
	SeedStatusDownloaded   = "downloaded"
	SeedStatusPublishing   = "publishing"
	SeedStatusPublished    = "published"
	SeedStatusCrossSeeding = "cross_seeding"
	SeedStatusSeeding      = "seeding"
	SeedStatusMonitoring   = "monitoring"
	SeedStatusRetired      = "retired"
	SeedStatusSkipped      = "skipped"
	SeedStatusFailed       = "failed"
	SeedStatusAbandoned    = "abandoned"
)

// Seed represents one row in the seeds table.
type Seed struct {
	ID           int64
	InfoHash     string
	SourceSite   string
	SourceID     *int64
	Title        *string
	Size         *int64
	Category     *string
	Promotion    *string
	PublishTime  *string
	DiscoveredAt *string
	DownloadedAt *string
	Status       string
	ErrorMsg     *string
	CreatedAt    string
	UpdatedAt    string
}

// SeedFilter carries optional filter criteria for List.
// Zero / empty fields are ignored.
type SeedFilter struct {
	SourceSite string
	Status     string
	Category   string
	Promotion  string
	Keyword    string // title LIKE '%keyword%'
}

// seedCols maps column name -> scan destination for a *Seed.
var seedCols = []struct {
	name string
	dst  func(s *Seed) interface{}
}{
	{"id", func(s *Seed) interface{} { return &s.ID }},
	{"info_hash", func(s *Seed) interface{} { return &s.InfoHash }},
	{"source_site", func(s *Seed) interface{} { return &s.SourceSite }},
	{"source_id", func(s *Seed) interface{} { return &s.SourceID }},
	{"title", func(s *Seed) interface{} { return &s.Title }},
	{"size", func(s *Seed) interface{} { return &s.Size }},
	{"category", func(s *Seed) interface{} { return &s.Category }},
	{"promotion", func(s *Seed) interface{} { return &s.Promotion }},
	{"publish_time", func(s *Seed) interface{} { return &s.PublishTime }},
	{"discovered_at", func(s *Seed) interface{} { return &s.DiscoveredAt }},
	{"downloaded_at", func(s *Seed) interface{} { return &s.DownloadedAt }},
	{"status", func(s *Seed) interface{} { return &s.Status }},
	{"error_msg", func(s *Seed) interface{} { return &s.ErrorMsg }},
	{"created_at", func(s *Seed) interface{} { return &s.CreatedAt }},
	{"updated_at", func(s *Seed) interface{} { return &s.UpdatedAt }},
}

func scanSeed(scanner interface{ Scan(dest ...interface{}) error }) (*Seed, error) {
	s := &Seed{}
	dests := make([]interface{}, len(seedCols))
	for i, c := range seedCols {
		dests[i] = c.dst(s)
	}
	if err := scanner.Scan(dests...); err != nil {
		return nil, err
	}
	return s, nil
}

// ---- CRUD ----

// InsertSeed inserts a new row into seeds. Returns the new row id.
// info_hash, source_site, and status (defaults to 'discovered') are
// required; other fields are optional.
func (s *RelayStore) InsertSeed(seed *Seed) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return 0, err
	}

	if seed.Status == "" {
		seed.Status = SeedStatusDiscovered
	}
	now := utcNow()
	if seed.CreatedAt == "" {
		seed.CreatedAt = now
	}
	if seed.UpdatedAt == "" {
		seed.UpdatedAt = now
	}

	const query = `INSERT INTO seeds (
		info_hash, source_site, source_id, title, size, category,
		promotion, publish_time, discovered_at, downloaded_at,
		status, error_msg, created_at, updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

	res, err := db.Exec(query,
		seed.InfoHash, seed.SourceSite, nilPtr(seed.SourceID), nilPtr(seed.Title),
		nilPtr(seed.Size), nilPtr(seed.Category), nilPtr(seed.Promotion),
		nilPtr(seed.PublishTime), nilPtr(seed.DiscoveredAt), nilPtr(seed.DownloadedAt),
		seed.Status, nilPtr(seed.ErrorMsg), seed.CreatedAt, seed.UpdatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert seed: %w", err)
	}
	return res.LastInsertId()
}

// GetSeedByHash returns a seed by (source_site, info_hash), or nil when not found.
func (s *RelayStore) GetSeedByHash(sourceSite, infoHash string) (*Seed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow("SELECT * FROM seeds WHERE source_site=? AND info_hash=?", sourceSite, infoHash)
	seed, scanErr := scanSeed(row)
	if scanErr == sql.ErrNoRows {
		return nil, nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("store: get seed by hash: %w", scanErr)
	}
	return seed, nil
}

// GetSeedsByStatus returns seeds with a given status, ordered by id.
func (s *RelayStore) GetSeedsByStatus(status string, limit, offset int) ([]*Seed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT * FROM seeds WHERE status=? ORDER BY id LIMIT ? OFFSET ?", status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: get seeds by status: %w", err)
	}
	defer rows.Close()
	var out []*Seed
	for rows.Next() {
		seed, err := scanSeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, seed)
	}
	return out, rows.Err()
}

// UpdateSeedStatus updates a seed's status and optional extra fields.
// extra may contain any seed column keyed by name (e.g. "downloaded_at",
// "error_msg", "promotion", …). updated_at is always refreshed.
func (s *RelayStore) UpdateSeedStatus(id int64, status string, extra map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return err
	}
	sets := []string{"status=?", "updated_at=?"}
	vals := []interface{}{status, utcNow()}
	allowed := map[string]bool{
		"downloaded_at": true, "error_msg": true, "promotion": true,
		"category": true, "title": true, "size": true,
		"publish_time": true, "discovered_at": true, "source_id": true,
	}
	for k, v := range extra {
		if !allowed[k] {
			return fmt.Errorf("store: 未知字段: %q", k)
		}
		sets = append(sets, k+"=?")
		vals = append(vals, v)
	}
	vals = append(vals, id)
	query := fmt.Sprintf("UPDATE seeds SET %s WHERE id=?", strings.Join(sets, ", "))
	if _, err := db.Exec(query, vals...); err != nil {
		return fmt.Errorf("store: update seed status: %w", err)
	}
	return nil
}

// ListSeeds returns seeds matching the filter, ordered by id desc.
func (s *RelayStore) ListSeeds(filter SeedFilter, limit, offset int) ([]*Seed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.requireOpen()
	if err != nil {
		return nil, err
	}
	where := []string{}
	vals := []interface{}{}
	if filter.SourceSite != "" {
		where = append(where, "source_site=?")
		vals = append(vals, filter.SourceSite)
	}
	if filter.Status != "" {
		where = append(where, "status=?")
		vals = append(vals, filter.Status)
	}
	if filter.Category != "" {
		where = append(where, "category=?")
		vals = append(vals, filter.Category)
	}
	if filter.Promotion != "" {
		where = append(where, "promotion=?")
		vals = append(vals, filter.Promotion)
	}
	if filter.Keyword != "" {
		where = append(where, "title LIKE ?")
		vals = append(vals, "%"+filter.Keyword+"%")
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	query := "SELECT * FROM seeds" + clause + " ORDER BY id DESC LIMIT ? OFFSET ?"
	vals = append(vals, limit, offset)

	rows, err := db.Query(query, vals...)
	if err != nil {
		return nil, fmt.Errorf("store: list seeds: %w", err)
	}
	defer rows.Close()
	var out []*Seed
	for rows.Next() {
		seed, err := scanSeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, seed)
	}
	return out, rows.Err()
}

// ---- helpers ----

func nilPtr[T any](v *T) interface{} {
	if v == nil {
		return nil
	}
	return *v
}
