package qb

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// types
// ---------------------------------------------------------------------------

// slowEntry tracks how long a torrent's download speed has been below the
// threshold, used by IsSlow.
type slowEntry struct {
	belowStart time.Time
}

// TorrentInfo is a typed representation of a qB torrent, carrying the
// fields most useful for relay monitoring and strategy decisions.
type TorrentInfo struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Category     string  `json:"category"`
	SavePath     string  `json:"save_path"`
	Error        string  `json:"-"`
	Size         int64   `json:"size"`
	DLSpeed      int64   `json:"dlspeed"`
	UPSpeed      int64   `json:"upspeed"`
	Downloaded   int64   `json:"downloaded"`
	Uploaded     int64   `json:"uploaded"`
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

// TrackerInfo holds tracker-level information for a single torrent.
type TrackerInfo struct {
	URL      string `json:"url"`
	Status   int    `json:"status"`
	Tier     int    `json:"tier"`
	NumPeers int    `json:"num_peers"`
	NumSeeds int    `json:"num_seeds"`
	Msg      string `json:"msg"`
}

// ---------------------------------------------------------------------------
// API response shims (only the fields we actually need)
// ---------------------------------------------------------------------------

type syncMainDataResp struct {
	ServerState struct {
		FreeSpaceOnDisk int64 `json:"free_space_on_disk"`
	} `json:"server_state"`
}

type transferInfoResp struct {
	DLSpeed int64 `json:"dl_info_speed"`
	UPSpeed int64 `json:"up_info_speed"`
	DLTotal int64 `json:"dl_info_data"`
	UPTotal int64 `json:"up_info_data"`
}

// ---------------------------------------------------------------------------
// GetDiskSpace
// ---------------------------------------------------------------------------

// GetDiskSpace returns the free disk space reported by qB (via
// /api/v2/sync/maindata).  Total and Used are populated only when known;
// on most setups only FreeOnDisk is filled.
func (q *QBittorrent) GetDiskSpace() (*DiskInfo, error) {
	params := url.Values{"rid": {"0"}}
	resp, err := q.get("/api/v2/sync/maindata", params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("获取磁盘空间失败(HTTP %d): %s", resp.StatusCode, text)
	}
	var s syncMainDataResp
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, reqErrf("解析磁盘空间响应失败: %v", err)
	}
	return &DiskInfo{FreeOnDisk: s.ServerState.FreeSpaceOnDisk}, nil
}

// ---------------------------------------------------------------------------
// GetTransferInfo
// ---------------------------------------------------------------------------

// GetTransferInfo returns global transfer statistics via
// /api/v2/transfer/info.
func (q *QBittorrent) GetTransferInfo() (*TransferInfo, error) {
	resp, err := q.get("/api/v2/transfer/info", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("获取传输信息失败(HTTP %d): %s", resp.StatusCode, text)
	}
	var t transferInfoResp
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, reqErrf("解析传输信息响应失败: %v", err)
	}
	return &TransferInfo{
		DLSpeed: t.DLSpeed,
		UPSpeed: t.UPSpeed,
		DLTotal: t.DLTotal,
		UPTotal: t.UPTotal,
	}, nil
}

// ---------------------------------------------------------------------------
// GetAllTorrents
// ---------------------------------------------------------------------------

// GetAllTorrents returns typed TorrentInfo for every torrent matching the
// optional filter string ("downloading", "seeding", "paused", "active",
// etc.).  An empty filter returns all torrents.
func (q *QBittorrent) GetAllTorrents(filter string) ([]*TorrentInfo, error) {
	params := url.Values{}
	if filter != "" {
		params.Set("filter", filter)
	}
	resp, err := q.get("/api/v2/torrents/info", params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode == http.StatusForbidden {
		return nil, reqErrf("info (filter=%q) 被拒(HTTP 403): %s", filter, text)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("info (filter=%q) 失败(HTTP %d): %s", filter, resp.StatusCode, text)
	}
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, reqErrf("info 响应解析失败: %v", err)
	}
	out := make([]*TorrentInfo, 0, len(raw))
	for _, m := range raw {
		ti := torrentInfoFromMap(m)
		out = append(out, ti)
	}
	return out, nil
}

func torrentInfoFromMap(m map[string]any) *TorrentInfo {
	return &TorrentInfo{
		Hash:         strVal(m, "hash"),
		Name:         strVal(m, "name"),
		State:        strVal(m, "state"),
		Category:     strVal(m, "category"),
		SavePath:     strVal(m, "save_path"),
		Size:         intVal(m, "size"),
		DLSpeed:      intVal(m, "dlspeed"),
		UPSpeed:      intVal(m, "upspeed"),
		Downloaded:   intVal(m, "downloaded"),
		Uploaded:     intVal(m, "uploaded"),
		Progress:     floatVal(m, "progress"),
		Ratio:        floatVal(m, "ratio"),
		AddedOn:      intVal(m, "added_on"),
		CompletionOn: intVal(m, "completion_on"),
		Seeders:      int(intVal(m, "num_complete")),
		Leechers:     int(intVal(m, "num_leechs")),
	}
}

// ---------------------------------------------------------------------------
// IsSlow
// ---------------------------------------------------------------------------

// IsSlow reports whether torrent hash has been downloading slower than
// thresholdKBps for at least duration. It uses internal per-hash state (a
// sliding "slow since" timer), so callers should invoke it periodically.
// API or state errors are treated as "not slow" (returns false).
func (q *QBittorrent) IsSlow(hash string, thresholdKBps int, duration time.Duration) bool {
	info, err := q.GetTorrent(hash)
	if err != nil || info == nil {
		return false
	}
	state, _ := info["state"].(string)
	if !strings.EqualFold(state, "downloading") && !strings.EqualFold(state, "stalledDL") {
		q.resetSlowTimer(hash)
		return false
	}
	speed := intVal(info, "dlspeed")
	threshold := int64(thresholdKBps) * 1024
	if speed >= threshold {
		q.resetSlowTimer(hash)
		return false
	}
	// speed is below threshold – start or advance the timer.
	q.slowMu.Lock()
	e, ok := q.slowTracker[hash]
	if !ok || e.belowStart.IsZero() {
		if !ok {
			e = &slowEntry{}
			q.slowTracker[hash] = e
		}
		e.belowStart = time.Now()
		q.slowMu.Unlock()
		return false
	}
	q.slowMu.Unlock()
	return time.Since(e.belowStart) >= duration
}

func (q *QBittorrent) resetSlowTimer(hash string) {
	q.slowMu.Lock()
	if e, ok := q.slowTracker[hash]; ok {
		e.belowStart = time.Time{}
	}
	q.slowMu.Unlock()
}

// ---------------------------------------------------------------------------
// WaitForCompletion
// ---------------------------------------------------------------------------

// WaitForCompletion polls the torrent status every second until its
// progress reaches 1 or timeout expires.  It returns nil on completion
// (state moves to uploading/seeding/etc.) and an error on timeout.
func (q *QBittorrent) WaitForCompletion(hash string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// immediate first check
	if done, err := q.isCompleted(hash); err != nil {
		return err
	} else if done {
		return nil
	}

	for {
		select {
		case <-ticker.C:
			if time.Now().After(deadline) {
				return reqErrf("等待种子 %s 完成超时(%v)", hash, timeout)
			}
			done, err := q.isCompleted(hash)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

func (q *QBittorrent) isCompleted(hash string) (bool, error) {
	info, err := q.GetTorrent(hash)
	if err != nil {
		return false, err
	}
	if info == nil {
		return false, reqErrf("种子 %s 不存在", hash)
	}
	progress, _ := info["progress"].(float64)
	if progress >= 1.0 {
		return true, nil
	}
	// Also treat "missingFiles" / "error" states with progress==1 as done
	state, _ := info["state"].(string)
	if state == "missingFiles" || state == "error" {
		return true, fmt.Errorf("种子 %s 进入错误状态: %s", truncate(hash, 12), state)
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// GetTorrentTrackers
// ---------------------------------------------------------------------------

// GetTorrentTrackers returns tracker information for the torrent identified
// by v1 infohash.
func (q *QBittorrent) GetTorrentTrackers(hash string) ([]TrackerInfo, error) {
	resp, err := q.get("/api/v2/torrents/trackers", url.Values{"hash": {hash}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := truncate(string(body), 200)
	if resp.StatusCode != http.StatusOK {
		return nil, reqErrf("获取 tracker 失败(HTTP %d): %s", resp.StatusCode, text)
	}
	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, reqErrf("解析 tracker 响应失败: %v", err)
	}
	out := make([]TrackerInfo, 0, len(raw))
	for _, m := range raw {
		out = append(out, TrackerInfo{
			URL:      strVal(m, "url"),
			Status:   int(intVal(m, "status")),
			Tier:     int(intVal(m, "tier")),
			NumPeers: int(intVal(m, "num_peers")),
			NumSeeds: int(intVal(m, "num_seeds")),
			Msg:      strVal(m, "msg"),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// postJSON – helper for JSON-body POST requests (used by autoinit, etc.)
// ---------------------------------------------------------------------------

// postJSON sends a POST request with a JSON body. The body value is
// marshalled and retried on auth failure (one re-login).
func (q *QBittorrent) postJSON(path string, body any) (*http.Response, error) {
	return q.request(http.MethodPost, path, nil, true, func() (io.Reader, string, error) {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, "", fmt.Errorf("json marshal: %w", err)
		}
		return strings.NewReader(string(data)), "application/json", nil
	})
}

// ---------------------------------------------------------------------------
// tiny helpers (typed extraction from map[string]any)
// ---------------------------------------------------------------------------

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func intVal(m map[string]any, key string) int64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		case json.Number:
			i, err := n.Int64()
			if err == nil {
				return i
			}
		}
	}
	return 0
}

func floatVal(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int64:
			return float64(n)
		case int:
			return float64(n)
		case json.Number:
			f, err := n.Float64()
			if err == nil {
				return f
			}
		}
	}
	return 0
}
