package qb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/autoseedrelay/relay/internal/bencode"
)

// 出站响应体读取上限:普通端点 20MB,qB torrents/info 列表 50MB。
const (
	maxBodyDefault = 20 << 20
	maxInfoBody    = 50 << 20
)

// readLimited reads resp.Body capped at limit bytes, returning a clear
// *http.MaxBytesError when the response exceeds the limit.
func readLimited(resp *http.Response, limit int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(nil, resp.Body, limit))
}

// completedSeedingStates is the set of qB states meaning "fully downloaded
// and actively seeding".
var completedSeedingStates = map[string]bool{
	"uploading": true,
	"stalledUP": true,
	"stoppedUP": true,
}

// slowEntryIdle is how long a slowTracker entry may sit untouched before
// IsSlow prunes it. This keeps the per-hash map bounded when hashes churn.
const slowEntryIdle = 10 * time.Minute

// slowEntry tracks how long a torrent's download speed has been below the
// threshold, used by IsSlow.
type slowEntry struct {
	belowStart time.Time // when the hash first fell below the threshold
	lastSeen   time.Time // last IsSlow/reset touch, used for idle pruning
}

// TorrentInfo is a typed representation of a qB torrent, carrying the
// fields most useful for relay monitoring and strategy decisions.
type TorrentInfo struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Category     string  `json:"category"`
	SavePath     string  `json:"save_path"`
	Size         int64   `json:"size"`
	DLSpeed      int64   `json:"dlspeed"`
	UPSpeed      int64   `json:"upspeed"`
	Downloaded   int64   `json:"downloaded"`
	Uploaded     int64   `json:"uploaded"`
	Completed    int64   `json:"completed"`
	Progress     float64 `json:"progress"`
	Ratio        float64 `json:"ratio"`
	AddedOn      int64   `json:"added_on"`
	CompletionOn int64   `json:"completion_on"`
	Seeders      int     `json:"num_complete"`
	Leechers     int     `json:"num_leechs"`
}

// DiskInfo holds disk-space information for the volume on which qB saves
// downloads.
type DiskInfo struct {
	FreeOnDisk int64 `json:"free_on_disk"`
	Total      int64 `json:"total"`
	Used       int64 `json:"used"`
}

// TransferInfo holds global transfer statistics (all torrents combined).
type TransferInfo struct {
	DLSpeed int64 `json:"dl_info_speed"`
	UPSpeed int64 `json:"up_info_speed"`
	DLTotal int64 `json:"dl_info_data"`
	UPTotal int64 `json:"up_info_data"`
}

// Info lists torrents as typed TorrentInfo. An optional filter string
// ("downloading", "seeding", "paused", "active", ...) narrows the result;
// an empty filter returns all torrents.
func (i *Instance) Info(ctx context.Context, filter string) ([]*TorrentInfo, error) {
	params := url.Values{}
	if filter != "" {
		params.Set("filter", filter)
	}
	return i.torrentsInfo(ctx, params)
}

func (i *Instance) torrentsInfo(ctx context.Context, params url.Values) ([]*TorrentInfo, error) {
	resp, err := i.get(ctx, "/api/v2/torrents/info", params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp, maxInfoBody)
	if err != nil {
		return nil, reqErrf("info 响应体超限(>50MB): %v", err)
	}
	text := truncate(string(body), 200)
	if resp.StatusCode == http.StatusForbidden {
		return nil, reqErrf("info 被拒(HTTP 403): %s", text)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("info 失败(HTTP %d): %s", resp.StatusCode, text)
	}
	var raw []TorrentInfo
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, reqErrf("info 响应解析失败: %v", err)
	}
	out := make([]*TorrentInfo, 0, len(raw))
	for idx := range raw {
		out = append(out, &raw[idx])
	}
	return out, nil
}

// GetTorrent returns a single torrent by v1 infohash, or nil when no such
// torrent exists.
func (i *Instance) GetTorrent(ctx context.Context, hash string) (*TorrentInfo, error) {
	lst, err := i.torrentsInfo(ctx, url.Values{"hashes": {hash}})
	if err != nil {
		return nil, err
	}
	if len(lst) > 0 {
		return lst[0], nil
	}
	return nil, nil
}

// ExportTorrent downloads the .torrent bytes for a single v1 infohash and
// verifies they decode as a valid torrent (a bencoded dict with an info
// key).
func (i *Instance) ExportTorrent(ctx context.Context, hash string) ([]byte, error) {
	resp, err := i.get(ctx, "/api/v2/torrents/export", url.Values{"hash": {hash}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp, maxBodyDefault)
	if err != nil {
		return nil, reqErrf("导出失败: 响应体超限: %v", err)
	}
	text := truncate(string(body), 200)
	if resp.StatusCode == http.StatusNotFound {
		return nil, reqErrf("导出失败(HTTP 404):hash %s 未找到或元数据缺失(has_metadata=false)", hash)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("导出失败(HTTP %d): %s", resp.StatusCode, text)
	}
	obj, err := bencode.Decode(body)
	if err != nil {
		return nil, reqErrf("导出的响应不是合法 .torrent(%v)", err)
	}
	d, ok := obj.(map[string]any)
	if !ok {
		return nil, reqErrf("导出的响应不是合法 .torrent(缺 info 字典)")
	}
	if _, ok := d["info"]; !ok {
		return nil, reqErrf("导出的响应不是合法 .torrent(缺 info 字典)")
	}
	return body, nil
}

// GetDiskSpace returns the free disk space reported by qB (via
// /api/v2/sync/maindata). Total and Used are populated only when known;
// on most setups only FreeOnDisk is filled.
func (i *Instance) GetDiskSpace(ctx context.Context) (*DiskInfo, error) {
	resp, err := i.get(ctx, "/api/v2/sync/maindata", url.Values{"rid": {"0"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp, maxBodyDefault)
	if err != nil {
		return nil, reqErrf("获取磁盘空间失败: 响应体超限: %v", err)
	}
	text := truncate(string(body), 200)
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("获取磁盘空间失败(HTTP %d): %s", resp.StatusCode, text)
	}
	var s struct {
		ServerState struct {
			FreeSpaceOnDisk int64 `json:"free_space_on_disk"`
		} `json:"server_state"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, reqErrf("解析磁盘空间响应失败: %v", err)
	}
	return &DiskInfo{FreeOnDisk: s.ServerState.FreeSpaceOnDisk}, nil
}

// GetTransferInfo returns global transfer statistics via
// /api/v2/transfer/info.
func (i *Instance) GetTransferInfo(ctx context.Context) (*TransferInfo, error) {
	resp, err := i.get(ctx, "/api/v2/transfer/info", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp, maxBodyDefault)
	if err != nil {
		return nil, reqErrf("获取传输信息失败: 响应体超限: %v", err)
	}
	text := truncate(string(body), 200)
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("获取传输信息失败(HTTP %d): %s", resp.StatusCode, text)
	}
	var t TransferInfo
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, reqErrf("解析传输信息响应失败: %v", err)
	}
	return &t, nil
}

// IsCompletedSeeding reports whether a torrent is fully downloaded and
// actively seeding: progress==1, completed>0, completion_on!=-1 and the
// state belongs to the completed-seeding set.
func IsCompletedSeeding(t *TorrentInfo) bool {
	if t == nil {
		return false
	}
	return t.Progress == 1 && t.Completed > 0 && t.CompletionOn != -1 && completedSeedingStates[t.State]
}

// IsSlow reports whether torrent hash has been downloading slower than
// thresholdKBps for at least duration. It uses internal per-hash state (a
// sliding "slow since" timer), so callers should invoke it periodically.
// API or state errors are treated as "not slow" (returns false).
//
// Every call also prunes slowTracker entries idle for longer than
// slowEntryIdle, keeping the map bounded as hashes churn. All read/modify/
// write of slowTracker happens under slowMu, so belowStart is never read
// outside the lock (fixes the IsSlow data race).
func (i *Instance) IsSlow(ctx context.Context, hash string, thresholdKBps int, duration time.Duration) bool {
	info, err := i.GetTorrent(ctx, hash)
	if err != nil || info == nil {
		return false
	}
	if !strings.EqualFold(info.State, "downloading") && !strings.EqualFold(info.State, "stalledDL") {
		i.resetSlowTimer(hash)
		return false
	}
	threshold := int64(thresholdKBps) * 1024
	if info.DLSpeed >= threshold {
		i.resetSlowTimer(hash)
		return false
	}

	now := time.Now()
	i.slowMu.Lock()
	defer i.slowMu.Unlock()

	i.pruneSlowTrackerLocked(now)

	e, ok := i.slowTracker[hash]
	if !ok {
		i.slowTracker[hash] = &slowEntry{belowStart: now, lastSeen: now}
		return false
	}
	e.lastSeen = now
	if e.belowStart.IsZero() {
		e.belowStart = now
		return false
	}
	return now.Sub(e.belowStart) >= duration
}

func (i *Instance) resetSlowTimer(hash string) {
	i.slowMu.Lock()
	defer i.slowMu.Unlock()
	if e, ok := i.slowTracker[hash]; ok {
		e.belowStart = time.Time{}
		e.lastSeen = time.Now()
	}
}

// pruneSlowTrackerLocked drops slowTracker entries whose lastSeen is older
// than slowEntryIdle. Caller must hold slowMu.
func (i *Instance) pruneSlowTrackerLocked(now time.Time) {
	for h, e := range i.slowTracker {
		if now.Sub(e.lastSeen) > slowEntryIdle {
			delete(i.slowTracker, h)
		}
	}
}
