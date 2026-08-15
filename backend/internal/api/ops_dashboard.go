package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// gb is the bytes-per-gigabyte divisor used to convert qB's byte counts.
const gb = 1 << 30

// processStart is the process uptime anchor. Captured at package init, which for
// the api package coincides with process start (the handler package is loaded
// before serving begins). It drives dashboard.status.uptime_seconds.
var processStart = time.Now()

// dashboard handles GET /dashboard → {status, stats, tasks, events, trend}.
func (o *Ops) dashboard(c *gin.Context) {
	if !o.requireRepo(c) {
		return
	}
	ctx := c.Request.Context()
	db := o.Repo.DB()

	// status.qbs — one entry per registered qB instance, plus the flat
	// qb_online / qb_total counts the frontend status bar renders.
	qbs := make([]gin.H, 0)
	var qbOnline, qbTotal int64
	if o.QB != nil {
		if statuses, err := o.QB.AllHealthy(ctx); err == nil {
			for _, st := range statuses {
				qbs = append(qbs, gin.H{"name": st.Name, "online": st.Online, "version": st.Version})
				qbTotal++
				if st.Online {
					qbOnline++
				}
			}
		}
	}

	// status.sources — all sources (active and paused). The Repo exposes only
	// GetActiveSources, so the full list is read directly.
	sources := make([]gin.H, 0)
	if rows, err := db.QueryContext(ctx, `SELECT id, name, status, fail_count FROM sources ORDER BY id`); err == nil {
		for rows.Next() {
			var id, fail int64
			var name, status string
			if rows.Scan(&id, &name, &status, &fail) == nil {
				sources = append(sources, gin.H{"id": id, "name": name, "status": status, "fail_count": fail})
			}
		}
		rows.Close()
	}

	// status.disk — sum GetDiskSpace across instances; per-instance failures
	// (offline qB) are ignored.
	var freeBytes, totalBytes int64
	if o.QB != nil {
		for _, name := range o.QB.Names() {
			inst, ok := o.QB.Get(name)
			if !ok {
				continue
			}
			d, err := inst.GetDiskSpace(ctx)
			if err != nil {
				continue
			}
			freeBytes += d.FreeOnDisk
			totalBytes += d.Total
		}
	}
	disk := gin.H{"free_gb": float64(freeBytes) / gb, "total_gb": float64(totalBytes) / gb}

	// status.engine — running + workers from the engine snapshot.
	engineStatus := gin.H{"running": false, "workers": 0}
	if o.Engine != nil {
		if s := o.Engine.Status(); s != nil {
			if v, ok := s["running"].(bool); ok {
				engineStatus["running"] = v
			}
			if v, ok := s["workers"].(int); ok {
				engineStatus["workers"] = v
			}
		}
	}

	// tasks — the 10 most recent seeds.
	tasks := make([]seedJSON, 0)
	if seeds, err := o.Repo.ListRecentSeeds(ctx, 10); err == nil {
		for _, s := range seeds {
			tasks = append(tasks, toSeedJSON(s))
		}
	}

	// events — the 10 most recent activity_log rows.
	events := make([]logJSON, 0)
	if rows, err := db.QueryContext(ctx,
		`SELECT id, seed_id, level, action, detail, created_at FROM activity_log ORDER BY id DESC LIMIT 10`); err == nil {
		for rows.Next() {
			var l logJSON
			var sid sql.NullInt64
			if rows.Scan(&l.ID, &sid, &l.Level, &l.Action, &l.Detail, &l.CreatedAt) == nil {
				l.SeedID = sid.Int64
				events = append(events, l)
			}
		}
		rows.Close()
	}

	c.JSON(http.StatusOK, gin.H{
		"status": gin.H{
			"qbs":            qbs,
			"qb_online":      qbOnline,
			"qb_total":       qbTotal,
			"sources":        sources,
			"disk":           disk,
			"uptime_seconds": int64(time.Since(processStart).Seconds()),
			"engine":         engineStatus,
		},
		"stats":  o.stats(ctx),
		"tasks":  tasks,
		"events": events,
		"trend":  o.trend(ctx),
	})
}

// stats aggregates the stat cards from seeds / relay_records via SQL. The
// today_* counters use COALESCE(published_at, updated_at): published_at is the
// semantically-correct column but the M2c pipeline does not yet stamp it, so the
// fallback keeps the counters non-zero on live data.
func (o *Ops) stats(ctx context.Context) gin.H {
	db := o.Repo.DB()
	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()

	count := func(query string, args ...any) int64 {
		var n int64
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return 0
		}
		return n
	}

	return gin.H{
		"total_published":    count(`SELECT COUNT(*) FROM relay_records WHERE role = 'publisher' AND status = 'published'`),
		"total_cross_seeded": count(`SELECT COUNT(*) FROM relay_records WHERE role = 'seeder' AND status = 'cross_seeding'`),
		"current_seeding":    count(`SELECT COUNT(*) FROM seeds WHERE status = 'seeding'`),
		"retry":              count(`SELECT COUNT(*) FROM seeds WHERE status = 'retry'`),
		"failed":             count(`SELECT COUNT(*) FROM seeds WHERE status = 'failed'`),
		"today_published": count(`SELECT COUNT(*) FROM relay_records
			WHERE role = 'publisher' AND status = 'published' AND COALESCE(published_at, updated_at) >= ?`, todayStart),
		"today_cross_seeded": count(`SELECT COUNT(*) FROM relay_records
			WHERE role = 'seeder' AND status = 'cross_seeding' AND COALESCE(published_at, updated_at) >= ?`, todayStart),
	}
}

// trend returns the per-day seed-discovery counts for the last 7 days (UTC),
// oldest first, always with 7 entries so the frontend can render a fixed bar.
func (o *Ops) trend(ctx context.Context) []gin.H {
	db := o.Repo.DB()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	counts := map[string]int64{}
	if rows, err := db.QueryContext(ctx,
		`SELECT date(discovered_at, 'unixepoch') AS d, COUNT(*) FROM seeds
		   WHERE discovered_at >= ? GROUP BY d`, today.AddDate(0, 0, -6).Unix()); err == nil {
		for rows.Next() {
			var d string
			var n int64
			if rows.Scan(&d, &n) == nil {
				counts[d] = n
			}
		}
		rows.Close()
	}

	trend := make([]gin.H, 0, 7)
	for i := 0; i < 7; i++ {
		day := today.AddDate(0, 0, i-6)
		d := day.Format("2006-01-02")
		trend = append(trend, gin.H{"date": d, "count": counts[d]})
	}
	return trend
}
