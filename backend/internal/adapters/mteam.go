package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/autoseedrelay/relay/internal/parser"
)

// MTeam adapts the M-Team Spring Boot API (x-api-key auth, JSON, multipart
// upload at POST /torrent/createOredit).
type MTeam struct {
	base
	teams map[string]int // probed team name -> id cache
}

func newMTeam(cfg SiteConfig) Adapter {
	cfg.Type = TypeMTeam
	return &MTeam{base: newBase(cfg)}
}

func (m *MTeam) uploadURL() string {
	return strings.TrimRight(m.cfg.BaseURL, "/") + "/torrent/createOredit"
}

func (m *MTeam) categoryListURL() string {
	return strings.TrimRight(m.cfg.BaseURL, "/") + "/torrent/categoryList"
}

func (m *MTeam) teamListURL() string {
	return strings.TrimRight(m.cfg.BaseURL, "/") + "/torrent/teamList"
}

func (m *MTeam) headers() map[string]string {
	h := baseHeaders(nil)
	h["x-api-key"] = m.cfg.APIToken
	return h
}

// Probe implements Adapter.Probe for the M-Team architecture.
func (m *MTeam) Probe(ctx context.Context) (ProbeResult, error) {
	return probeMTeam(ctx, m.client, m.cfg.BaseURL, m.headers())
}

// Publish uploads a cleaned torrent to M-Team.
func (m *MTeam) Publish(ctx context.Context, tor *parser.ParsedTorrent, p PublishParams) (PublishResult, error) {
	url := m.uploadURL()
	if m.cfg.TestMode {
		return PublishResult{TestMode: true, Detail: "test mode: would POST " + url},
			newAdapterError(ErrTestMode, 0, "test mode: publish skipped (would POST "+url+")", "")
	}

	category, err := m.resolveCategory(p)
	if err != nil {
		return PublishResult{}, err
	}

	fields := map[string]any{
		"name":      p.Title,
		"descr":     p.Description,
		"category":  category,
		"anonymous": p.Anonymous,
	}
	if p.SubTitle != "" {
		fields["smallDescr"] = p.SubTitle // M-Team uses camelCase
	}
	if imdb := ExtractIMDB(p.IMDb); imdb != "" {
		fields["imdb"] = imdb // keep the tt prefix
	}
	if p.Douban != "" {
		fields["douban"] = p.Douban
	}
	if p.MediaInfo != "" {
		fields["mediainfo"] = p.MediaInfo
	}

	dimFields := []struct {
		kind  string
		field string
	}{
		{"standard", "standard"},
		{"codec", "videoCodec"},
		{"audiocodec", "audioCodec"},
		{"source", "source"},
		{"medium", "medium"},
		{"processing", "processing"},
	}
	for _, d := range dimFields {
		token := dimensionToken(p.Dimensions, d.kind)
		if token == "" {
			continue
		}
		if id, ok := resolveDimID(m.cfg.DimensionOverrides, d.kind, token); ok {
			fields[d.field] = id
		}
	}
	team := dimensionToken(p.Dimensions, "team")
	if team == "" {
		team = p.Team
	}
	if team != "" {
		if id, ok := resolveDimID(m.cfg.DimensionOverrides, "team", team); ok {
			fields["team"] = id
		} else if id, ok := m.teams[normToken(team)]; ok {
			fields["team"] = id
		}
	}

	if labels := nonEmpty(p.Labels); len(labels) > 0 {
		fields["labels"] = labels
	}
	if tags := mapTags(p.Tags, m.cfg.TagsMap); len(tags) > 0 {
		fields["tags"] = tags
	}
	if countries := nonEmpty(p.Countries); len(countries) > 0 {
		fields["countries"] = countries
	}

	fileBytes, err := encodeTorrent(tor)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, 0, "encode torrent: "+err.Error(), "")
	}
	resp, err := postMultipart(ctx, m.client, url, m.headers(), fields, "file", torrentFilename(tor.Name), fileBytes, mteamConvert)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, 0, "upload request failed: "+err.Error(), err.Error())
	}
	body, err := readBody(resp)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, resp.StatusCode, "read response: "+err.Error(), "")
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || isMTeamAuthBody(body) {
		return PublishResult{}, newAdapterError(ErrAuthExpired, resp.StatusCode,
			"mteam rejected the x-api-key", body)
	}
	if isDuplicateBody(body, resp.StatusCode, mteamExistingHints) {
		return PublishResult{}, newAdapterError(ErrDuplicate, resp.StatusCode,
			"mteam reports the torrent already exists", body)
	}
	if resp.StatusCode >= 400 {
		return PublishResult{}, newAdapterError(nil, resp.StatusCode,
			fmt.Sprintf("mteam upload failed: HTTP %d", resp.StatusCode), body)
	}

	code, id := parseMTeamResult(body)
	ok := code == 0 || id != 0
	if !ok {
		return PublishResult{}, newAdapterError(nil, resp.StatusCode,
			fmt.Sprintf("mteam upload rejected: code=%d", code), body)
	}
	return PublishResult{OK: true, TargetID: id, Detail: fmt.Sprintf("mteam upload ok code=%d target_id=%d", code, id)}, nil
}

// mteamExistingHints includes the traditional-Chinese de-duplication markers.
var mteamExistingHints = []string{
	"duplicate",
	"exists",
	"existed",
	"repeat upload",
	"already uploaded",
	"種子已存在",
	"种子已存在",
	"重复发布",
	"重複發布",
	"已存在",
	"已上传过",
	"已上傳過",
}

func isMTeamAuthBody(body string) bool {
	low := strings.ToLower(body)
	for _, h := range []string{"unauthorized", "未授权", "invalid api key", "invalid token", "api key", "forbidden"} {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

// parseMTeamResult parses M-Team's {"code":0,"data":{"id":N}} envelope.
func parseMTeamResult(body string) (code int64, id int64) {
	code = -1
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return code, 0
	}
	if c, ok := data["code"]; ok && c != nil {
		if n, err := strconv.ParseInt(fmt.Sprintf("%v", c), 10, 64); err == nil {
			code = n
		}
	}
	id = extractID(body)
	return code, id
}

// requestJSON performs an authenticated JSON request against a read-only M-Team
// endpoint and returns the decoded object.
func (m *MTeam) requestJSON(ctx context.Context, url, method string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range m.headers() {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxResponseBody)
	if err != nil {
		return nil, fmt.Errorf("mteam endpoint %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mteam endpoint %s: HTTP %d", url, resp.StatusCode)
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return data, nil
}
