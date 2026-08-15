package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/autoseedrelay/relay/internal/adapters"
	"github.com/autoseedrelay/relay/internal/bencode"
	"github.com/autoseedrelay/relay/internal/descr"
	"github.com/autoseedrelay/relay/internal/mteam"
	"github.com/autoseedrelay/relay/internal/notifier"
	"github.com/autoseedrelay/relay/internal/parser"
	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
	"github.com/autoseedrelay/relay/internal/titler"
)

// adapterType maps a store.Target's type+version to an adapter architecture.
func adapterType(t *store.Target) string {
	switch t.Type {
	case "mteam":
		return adapters.TypeMTeam
	case "nexusphp_classic":
		return adapters.TypeNexusPHPClassic
	case "nexusphp":
		if t.Version == "classic" {
			return adapters.TypeNexusPHPClassic
		}
		return adapters.TypeNexusPHPAPI
	default:
		return t.Type // let adapters.New report the unknown type
	}
}

// siteConfigFromTarget translates a store.Target into an adapters.SiteConfig,
// parsing the JSON override columns and the fallback category.
func siteConfigFromTarget(t *store.Target) (adapters.SiteConfig, error) {
	cfg := adapters.SiteConfig{
		Name:     t.Name,
		Type:     adapterType(t),
		BaseURL:  t.BaseURL,
		Announce: t.AnnounceURL,
		Passkey:  t.Passkey,
		Cookie:   t.Cookie,
		APIToken: t.APIToken,
		TestMode: t.TestMode != 0,
	}
	if s := strings.TrimSpace(t.CategoryOverrides); s != "" && s != "{}" {
		if err := json.Unmarshal([]byte(s), &cfg.CategoryOverrides); err != nil {
			return cfg, fmt.Errorf("target %s: parse category_overrides: %w", t.Name, err)
		}
	}
	if s := strings.TrimSpace(t.DimensionOverrides); s != "" && s != "{}" {
		if err := json.Unmarshal([]byte(s), &cfg.DimensionOverrides); err != nil {
			return cfg, fmt.Errorf("target %s: parse dimension_overrides: %w", t.Name, err)
		}
	}
	if s := strings.TrimSpace(t.TagsMap); s != "" && s != "{}" {
		if err := json.Unmarshal([]byte(s), &cfg.TagsMap); err != nil {
			// A malformed tags_map must not fail the whole publish: degrade to
			// an empty map and surface the problem in the log.
			slog.Default().Warn("parse target tags_map failed; falling back to empty map",
				"target", t.Name, "err", err)
			cfg.TagsMap = map[string]string{}
		}
	}
	if f := strings.TrimSpace(t.FallbackCategory); f != "" {
		if n, err := strconv.Atoi(f); err == nil {
			cfg.FallbackCategory = &n
		}
	}
	return cfg, nil
}

// publishToOneTarget cleans the torrent for one target and publishes it,
// handling the atomic claim (抢发), duplicate → cross-seed, and auth-expired →
// warning + mark-failed. It returns nil on published or cross-seeded, otherwise
// a descriptive error.
func (p *RelayOne) publishToOneTarget(ctx context.Context, seed *store.Seed, src *store.Source, detail *source.SeedDetail, tor *parser.ParsedTorrent, t *store.Target) error {
	cfg, err := siteConfigFromTarget(t)
	if err != nil {
		return err
	}
	adapter, err := p.adapters(cfg)
	if err != nil {
		return fmt.Errorf("build adapter: %w", err)
	}
	cleaned, err := parser.CleanTorrentForTarget(tor, adapter.Announce(), "["+src.Name+"]")
	if err != nil {
		return fmt.Errorf("clean torrent: %w", err)
	}

	// Claim or reuse the relay record. A finished record (already published /
	// cross-seeded / retired) is filtered out by Relay before this point; this
	// check is a defensive backstop.
	existing, err := p.repo.GetRecord(ctx, seed.ID, t.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != nil && doneRecordStatuses[existing.Status] {
		return nil // already done
	}

	if existing == nil {
		inserted, err := p.repo.UpsertRecord(ctx, &store.RelayRecord{
			SeedID:   seed.ID,
			TargetID: t.ID,
			Role:     rolePublisher,
			Status:   statusPending,
		})
		if err != nil {
			return err
		}
		if !inserted {
			// Another instance won the atomic claim → degrade to cross-seeder.
			_ = p.repo.SetRecordRole(ctx, seed.ID, t.ID, roleSeeder)
			return p.finishCrossSeed(ctx, seed, t, adapter, tor)
		}
	}

	res, err := adapter.Publish(ctx, cleaned, buildPublishParams(seed, detail, tor, t))
	if err == nil {
		if !res.OK {
			_ = p.repo.UpdateRecordAttempt(ctx, seed.ID, t.ID, statusFailed, source.RedactError(res.Detail))
			return fmt.Errorf("publish reported not-ok: %s", res.Detail)
		}
		_ = p.repo.UpdateRecordStatus(ctx, seed.ID, t.ID, statusPublished, "")
		_ = p.repo.AppendLogSeed(ctx, seed.ID, "info", "published",
			fmt.Sprintf("published to %s target_id=%d", t.Name, res.TargetID))
		p.notify(ctx, notifier.LevelInfo, "发布成功",
			fmt.Sprintf("seed %d 发布到 %s (target_id=%d)", seed.ID, t.Name, res.TargetID), "published")
		return nil
	}

	if adapters.IsDuplicate(err) {
		_ = p.repo.SetRecordRole(ctx, seed.ID, t.ID, roleSeeder)
		return p.finishCrossSeed(ctx, seed, t, adapter, tor)
	}

	if adapters.IsAuthExpired(err) {
		_ = p.repo.UpdateRecordAttempt(ctx, seed.ID, t.ID, statusFailed, source.RedactError(err.Error()))
		_ = p.repo.AppendLogSeed(ctx, seed.ID, "warning", "auth_expired",
			fmt.Sprintf("%s: %s", t.Name, source.RedactError(err.Error())))
		p.notify(ctx, notifier.LevelWarning, "目标站鉴权过期",
			fmt.Sprintf("seed %d -> %s: %s", seed.ID, t.Name, source.RedactError(err.Error())), "auth_expired")
		return fmt.Errorf("auth expired: %w", err)
	}

	// category mismatch / transport / any other error.
	_ = p.repo.UpdateRecordAttempt(ctx, seed.ID, t.ID, statusFailed, source.RedactError(err.Error()))
	_ = p.repo.AppendLogSeed(ctx, seed.ID, "warning", "publish_failed",
		fmt.Sprintf("%s: %s", t.Name, source.RedactError(err.Error())))
	p.notify(ctx, notifier.LevelWarning, "发布失败",
		fmt.Sprintf("seed %d -> %s: %s", seed.ID, t.Name, source.RedactError(err.Error())), "publish_failed")
	return err
}

// finishCrossSeed runs the cross-seed join and records the outcome
// (cross_seeding on success, failed on error). It is shared by the
// duplicate-detected and the lost-claim paths.
func (p *RelayOne) finishCrossSeed(ctx context.Context, seed *store.Seed, t *store.Target, adapter adapters.Adapter, tor *parser.ParsedTorrent) error {
	if cerr := p.crossSeed(ctx, seed, t, adapter, tor); cerr != nil {
		_ = p.repo.UpdateRecordAttempt(ctx, seed.ID, t.ID, statusFailed, source.RedactError(cerr.Error()))
		return fmt.Errorf("cross-seed: %w", cerr)
	}
	_ = p.repo.UpdateRecordStatus(ctx, seed.ID, t.ID, statusCrossSeeded, "")
	_ = p.repo.AppendLogSeed(ctx, seed.ID, "info", "cross_seeding",
		fmt.Sprintf("cross-seeded to %s (duplicate)", t.Name))
	p.notify(ctx, notifier.LevelInfo, "交叉辅种",
		fmt.Sprintf("seed %d 在 %s 已存在，已交叉辅种", seed.ID, t.Name), "cross_seeded")
	return nil
}

// crossSeed joins the target's existing swarm (BIZ-SPEC §4): it re-announces
// the source torrent to the target announce (info_hash preserved), adds it to
// qB with skip_checking, and polls for completion within the timeout, falling
// back to a normal (re-checking) add on failure. It records the cross replica
// so the engine can retire it later.
func (p *RelayOne) crossSeed(ctx context.Context, seed *store.Seed, t *store.Target, adapter adapters.Adapter, tor *parser.ParsedTorrent) error {
	inst, qbID := p.selectQB(ctx, p.originQBName(ctx, seed))
	if inst == nil {
		return fmt.Errorf("no qB instance available for cross-seed")
	}

	re, err := parser.ReannounceTorrent(tor, adapter.Announce())
	if err != nil {
		return err
	}
	if !strings.EqualFold(re.InfoHash, seed.InfoHash) {
		return fmt.Errorf("reannounced info_hash %s != seed %s", re.InfoHash, seed.InfoHash)
	}
	data, err := bencode.Encode(re.RawDict)
	if err != nil {
		return fmt.Errorf("encode cross-seed torrent: %w", err)
	}
	name := re.Name + ".torrent"

	// Preferred: skip_checking against the already-present data directory.
	if _, err := inst.AddTorrentFile(ctx, name, data, qb.AddOptions{SkipChecking: true}); err != nil {
		if _, err2 := inst.AddTorrentFile(ctx, name, data, qb.AddOptions{}); err2 != nil {
			return fmt.Errorf("add cross-seed torrent: %v", err2)
		}
	}

	if err := p.waitCrossSeeded(ctx, inst, re.InfoHash); err != nil {
		// Verification did not complete in time: fall back to a normal
		// (re-checking) add so qB downloads/verifies the missing pieces.
		_ = inst.Delete(ctx, re.InfoHash, false)
		if _, err2 := inst.AddTorrentFile(ctx, name, data, qb.AddOptions{}); err2 != nil {
			return fmt.Errorf("cross-seed fallback add: %v", err2)
		}
	}

	if err := p.repo.UpsertReplica(ctx, &store.Replica{
		SeedID:   seed.ID,
		QBID:     qbID,
		InfoHash: re.InfoHash,
		Role:     replicaCross,
		Status:   replicaSeeding,
		Progress: 1,
	}); err != nil {
		return err
	}
	return nil
}

// waitCrossSeeded polls qB until the torrent's progress reaches 1 (the
// skip_checking verification) or the injectable deadline elapses.
func (p *RelayOne) waitCrossSeeded(ctx context.Context, inst *qb.Instance, hash string) error {
	deadline := p.now().Add(p.crossSeedTimeout)
	for {
		if !p.now().Before(deadline) {
			return fmt.Errorf("cross-seed verification timed out after %s", p.crossSeedTimeout)
		}
		if ti, err := inst.GetTorrent(ctx, hash); err == nil && ti != nil && ti.Progress >= 1 {
			return nil
		}
		if err := sleepCtx(ctx, p.crossSeedInterval); err != nil {
			return err
		}
	}
}

// buildPublishParams derives the target publish fields: structured title
// (M-Team normalized), dimensions (via titler.StandardKeys), category, tags,
// IMDb and the rebuilt description.
func buildPublishParams(seed *store.Seed, detail *source.SeedDetail, tor *parser.ParsedTorrent, t *store.Target) adapters.PublishParams {
	comps := titler.ParseTitle(seed.Title)
	keys := titler.StandardKeys(comps)

	title := seed.Title
	subtitle := detail.SmallDescr

	if adapterType(t) == adapters.TypeMTeam {
		mt := mteam.CleanMTteamTitle(seed.Title)
		title = mt.Name
		if mt.SmallDescrCN != "" && subtitle == "" {
			subtitle = mt.SmallDescrCN
		}
	}

	group := ""
	if comps.Group != nil {
		group = *comps.Group
	}

	dims := map[string]string{}
	if v := deref(keys["resolution"]); v != "" {
		dims["standard"] = v
	}
	if v := deref(keys["video_codec"]); v != "" {
		dims["codec"] = v
	}
	if v := deref(keys["audio_codec"]); v != "" {
		dims["audiocodec"] = v
	}
	if v := deref(keys["source"]); v != "" {
		dims["source"] = v
	}
	if v := deref(keys["medium"]); v != "" {
		dims["medium"] = v
	}
	if group != "" {
		dims["team"] = group
	}

	category := strings.TrimSpace(seed.Category)
	if category == "" {
		category = deref(keys["category"])
	}
	if category == "" {
		category = detail.CategoryName
	}

	return adapters.PublishParams{
		Title:       title,
		SubTitle:    subtitle,
		Description: buildDescription(detail, tor),
		Category:    category,
		Tags:        detail.Tags,
		IMDb:        detail.IMDb,
		Dimensions:  dims,
		Team:        group,
		MediaInfo:   detail.MediaInfo,
	}
}

var imdbLinkRe = regexp.MustCompile(`^tt\d{6,}$`)

// buildDescription rebuilds the target description from the source detail and
// the parsed torrent's file list, appending a validated IMDb link.
func buildDescription(detail *source.SeedDetail, tor *parser.ParsedTorrent) string {
	body := descr.NormalizeDescription(detail.DescrHTML)
	body = descr.StripSourceReferences(body)

	var fileList []map[string]any
	for _, f := range tor.Files {
		fileList = append(fileList, map[string]any{"path": f.Path, "size": f.Size})
	}

	var extra [][2]string
	if imdb := strings.ToLower(strings.TrimSpace(detail.IMDb)); imdbLinkRe.MatchString(imdb) {
		extra = append(extra, [2]string{
			"IMDb",
			`<a href="https://www.imdb.com/title/` + imdb + `/" target="_blank">` + imdb + `</a>`,
		})
	}

	return descr.BuildDescription(body, fileList, detail.SmallDescr, extra)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
