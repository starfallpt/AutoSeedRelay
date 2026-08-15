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

// NexusPHPAPI adapts NexusPHP >= 1.9 sites exposing the Laravel/Sanctum JSON
// API (POST /api/v1/upload, GET /api/v1/sections).
type NexusPHPAPI struct {
	base
}

func newNexusPHPAPI(cfg SiteConfig) Adapter {
	cfg.Type = TypeNexusPHPAPI
	return &NexusPHPAPI{base: newBase(cfg)}
}

func (n *NexusPHPAPI) UploadURL() string {
	return strings.TrimRight(n.cfg.BaseURL, "/") + "/api/v1/upload"
}

func (n *NexusPHPAPI) sectionsURL() string {
	return strings.TrimRight(n.cfg.BaseURL, "/") + "/api/v1/sections"
}

func (n *NexusPHPAPI) headers() map[string]string {
	h := baseHeaders(nil)
	if n.cfg.APIToken != "" {
		h["Authorization"] = "Bearer " + n.cfg.APIToken
	}
	return h
}

// Probe implements Adapter.Probe for the NexusPHP API architecture.
func (n *NexusPHPAPI) Probe(ctx context.Context) (ProbeResult, error) {
	return probeNexusPHP(ctx, n.client, n.cfg.BaseURL, n.headers())
}

// Publish uploads a cleaned torrent via the Laravel upload endpoint.
func (n *NexusPHPAPI) Publish(ctx context.Context, tor *parser.ParsedTorrent, p PublishParams) (PublishResult, error) {
	url := n.UploadURL()
	if n.cfg.TestMode {
		return PublishResult{TestMode: true, Detail: "test mode: would POST " + url},
			newAdapterError(ErrTestMode, 0, "test mode: publish skipped (would POST "+url+")", "")
	}

	category, err := n.resolveCategory(p)
	if err != nil {
		return PublishResult{}, err
	}

	fields := map[string]any{
		"name":  p.Title,
		"descr": p.Description,
		"type":  category,
	}
	if p.SubTitle != "" {
		fields["small_descr"] = p.SubTitle
	}
	if imdb := ExtractIMDB(p.IMDb); imdb != "" {
		fields["url"] = strings.TrimPrefix(imdb, "tt") // NexusPHP stores digits only
	}
	if p.MediaInfo != "" {
		fields["mediainfo"] = p.MediaInfo
	}
	if p.Uplver != "" {
		fields["uplver"] = p.Uplver
	}

	// Dimensions: only submit values resolvable to the site's integer IDs
	// (via DimensionOverrides or numeric passthrough). Never hard-code a site
	// taxonomy — the operator configures it.
	dimFields := []struct {
		kind  string
		field string
	}{
		{"source", "source"},
		{"medium", "medium"},
		{"codec", "codec"},
		{"standard", "standard"},
		{"audiocodec", "audiocodec"},
		{"processing", "processing"},
	}
	for _, d := range dimFields {
		token := dimensionToken(p.Dimensions, d.kind)
		if token == "" {
			continue
		}
		if id, ok := resolveDimID(n.cfg.DimensionOverrides, d.kind, token); ok {
			fields[d.field] = id
		}
	}
	team := dimensionToken(p.Dimensions, "team")
	if team == "" {
		team = p.Team
	}
	if team != "" {
		if id, ok := resolveDimID(n.cfg.DimensionOverrides, "team", team); ok {
			fields["team"] = id
		}
	}

	// Tags: API sites take a repeated tags[] field of numeric IDs (mapped via
	// TagsMap); unmapped tags are dropped.
	if tags := mapTags(p.Tags, n.cfg.TagsMap); len(tags) > 0 {
		fields["tags[]"] = tags
	}

	fileBytes, err := encodeTorrent(tor)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, 0, "encode torrent: "+err.Error(), "")
	}
	resp, err := postMultipart(ctx, n.client, url, n.headers(), fields, "file", torrentFilename(tor.Name), fileBytes, pythonStr)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, 0, "upload request failed: "+err.Error(), err.Error())
	}
	body, err := readBody(resp)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, resp.StatusCode, "read response: "+err.Error(), "")
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return PublishResult{}, newAdapterError(ErrAuthExpired, resp.StatusCode,
			"nexusphp API rejected the token (HTTP 401)", body)
	}
	if isDuplicateBody(body, resp.StatusCode, nexusphpExistingHints) {
		return PublishResult{}, newAdapterError(ErrDuplicate, resp.StatusCode,
			"nexusphp API reports the torrent already exists", body)
	}
	if resp.StatusCode >= 400 {
		return PublishResult{}, newAdapterError(nil, resp.StatusCode,
			fmt.Sprintf("nexusphp upload failed: HTTP %d", resp.StatusCode), body)
	}

	id := extractID(body)
	return PublishResult{OK: id != 0, TargetID: id, Detail: fmt.Sprintf("nexusphp upload ok target_id=%d", id)}, nil
}

// nexusphpExistingHints are server de-duplication response markers.
var nexusphpExistingHints = []string{
	"torrent_existed",
	"torrent already exists",
	"already exists",
	"duplicate torrent",
	"existed",
}

// extractID pulls the target torrent id out of a JSON response body. It
// understands {"data":{"id":N}}, {"data":N} and a top-level "id".
func extractID(body string) int64 {
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return 0
	}
	if d, ok := data["data"]; ok {
		if id := idFromValue(d); id != 0 {
			return id
		}
	}
	return idFromValue(data["id"])
}

func idFromValue(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i
		}
	case map[string]any:
		return idFromValue(n["id"])
	}
	return 0
}

// isDuplicateBody reports whether the status code (409) or the body text
// signals a server-side duplicate.
func isDuplicateBody(body string, statusCode int, hints []string) bool {
	if statusCode == http.StatusConflict {
		return true
	}
	low := strings.ToLower(body)
	for _, h := range hints {
		if strings.Contains(low, strings.ToLower(h)) {
			return true
		}
	}
	return false
}
