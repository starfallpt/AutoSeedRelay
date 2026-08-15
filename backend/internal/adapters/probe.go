package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// ProbeResult is the read-only site specification discovered by Probe. It is
// what the config wizard turns into SiteConfig (category overrides, dimension
// overrides, tag map) and what adapters cache for publish-time resolution.
type ProbeResult struct {
	// Type is the detected adapter type (one of the Type* constants).
	Type string `json:"type"`
	// BaseURL is the probed base URL.
	BaseURL string `json:"base_url"`
	// Auth is the auth mechanism: "bearer" (nexusphp), "cookie" (classic) or
	// "x-api-key" (mteam).
	Auth string `json:"auth,omitempty"`
	// Categories maps a category name (any language) to its numeric ID.
	Categories map[string]int `json:"categories"`
	// Tags enumerates the site's tags (id + name).
	Tags []Option `json:"tags,omitempty"`
	// Codecs enumerates the site's dimension taxonomies by kind
	// (standard / codec / audiocodec / source / medium / team / processing).
	Codecs map[string][]Option `json:"codecs,omitempty"`
	// Sections is the raw sections JSON for the NexusPHP API (kept whole so
	// the wizard can inspect any extra structure).
	Sections map[string]any `json:"sections,omitempty"`
	// Note carries a non-fatal observation (e.g. "auth required").
	Note string `json:"note,omitempty"`
}

// Probe auto-detects the site architecture and returns its live enums. It is
// read-only: it only GETs /api/v1/sections and upload.php, and POSTs the
// read-only /torrent/categoryList endpoint — never an upload endpoint.
func Probe(ctx context.Context, baseURL string, client *http.Client) (ProbeResult, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ProbeResult{}, newAdapterError(nil, 0, "probe: base URL is empty", "")
	}
	if client == nil {
		client = newHTTPClient(DefaultTimeout)
	}
	c := noRedirectClient(client)

	// 1) NexusPHP Laravel API: /api/v1/sections returns JSON.
	sectionsURL := baseURL + "/api/v1/sections"
	if resp, err := doProbeGet(ctx, c, sectionsURL); err == nil {
		if resp.StatusCode == http.StatusOK {
			body, _ := readLimited(resp.Body, maxResponseBody)
			resp.Body.Close()
			var sections map[string]any
			if json.Unmarshal(body, &sections) == nil {
				return nexusphpResult(baseURL, sections), nil
			}
		} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			return ProbeResult{Type: TypeNexusPHPAPI, BaseURL: baseURL, Auth: "bearer",
				Note: "sections endpoint exists but rejected the request (auth required)"}, nil
		}
		resp.Body.Close()
	}

	// 2) Classic NexusPHP form: upload.php with action="takeupload.php".
	uploadPageURL := baseURL + "/upload.php"
	if resp, err := doProbeGet(ctx, c, uploadPageURL); err == nil {
		body, _ := readLimited(resp.Body, maxResponseBody)
		resp.Body.Close()
		bodyStr := string(body)
		if resp.StatusCode == http.StatusOK && strings.Contains(bodyStr, "takeupload.php") {
			return classicResult(baseURL, bodyStr), nil
		}
		if isClassicLoginRedirect(resp, bodyStr) {
			return ProbeResult{Type: TypeNexusPHPClassic, BaseURL: baseURL, Auth: "cookie",
				Note: "upload.php present but requires login (cookie)"}, nil
		}
	}

	// 3) M-Team Spring API: POST /torrent/categoryList returns JSON.
	categoryListURL := baseURL + "/torrent/categoryList"
	if resp, err := doProbePost(ctx, c, categoryListURL); err == nil {
		body, _ := readLimited(resp.Body, maxResponseBody)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && json.Valid(body) {
			return mteamResult(baseURL, string(body)), nil
		}
	}

	return ProbeResult{}, newAdapterError(nil, 0,
		fmt.Sprintf("probe: cannot determine site architecture for %q (tried /api/v1/sections, /upload.php, /torrent/categoryList)", baseURL), "")
}

// noRedirectClient returns a copy of client that does not follow redirects, so
// probe can observe 3xx/401/404 statuses directly. Transport/Jar are shared.
func noRedirectClient(client *http.Client) *http.Client {
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	return &c
}

func doProbeGet(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AutoSeedRelay/0.2 (+relay script)")
	return c.Do(req)
}

func doProbePost(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AutoSeedRelay/0.2 (+relay script)")
	return c.Do(req)
}

// ---------------------------------------------------------------------------
// Per-architecture probe implementations (used by Adapter.Probe)
// ---------------------------------------------------------------------------

func probeNexusPHP(ctx context.Context, c *http.Client, baseURL string, headers map[string]string) (ProbeResult, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/sections", nil)
	if err != nil {
		return ProbeResult{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return ProbeResult{}, newAdapterError(nil, 0, "probe sections: "+err.Error(), err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ProbeResult{Type: TypeNexusPHPAPI, BaseURL: baseURL, Auth: "bearer",
			Note: "sections endpoint rejected the token (auth expired?)"}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, newAdapterError(nil, resp.StatusCode, "probe sections: HTTP "+strconv.Itoa(resp.StatusCode), "")
	}
	body, _ := readLimited(resp.Body, maxResponseBody)
	var sections map[string]any
	if err := json.Unmarshal(body, &sections); err != nil {
		return ProbeResult{}, err
	}
	return nexusphpResult(baseURL, sections), nil
}

func probeClassic(ctx context.Context, c *http.Client, baseURL string, headers map[string]string) (ProbeResult, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/upload.php", nil)
	if err != nil {
		return ProbeResult{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return ProbeResult{}, newAdapterError(nil, 0, "probe upload.php: "+err.Error(), err.Error())
	}
	defer resp.Body.Close()
	body, _ := readLimited(resp.Body, maxResponseBody)
	bodyStr := string(body)
	if isClassicLoginRedirect(resp, bodyStr) {
		return ProbeResult{Type: TypeNexusPHPClassic, BaseURL: baseURL, Auth: "cookie",
			Note: "upload.php requires login (cookie)"}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, newAdapterError(nil, resp.StatusCode, "probe upload.php: HTTP "+strconv.Itoa(resp.StatusCode), "")
	}
	return classicResult(baseURL, bodyStr), nil
}

func probeMTeam(ctx context.Context, c *http.Client, baseURL string, headers map[string]string) (ProbeResult, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/torrent/categoryList", nil)
	if err != nil {
		return ProbeResult{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return ProbeResult{}, newAdapterError(nil, 0, "probe categoryList: "+err.Error(), err.Error())
	}
	defer resp.Body.Close()
	body, _ := readLimited(resp.Body, maxResponseBody)
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, newAdapterError(nil, resp.StatusCode, "probe categoryList: HTTP "+strconv.Itoa(resp.StatusCode), "")
	}
	res := mteamResult(baseURL, string(body))

	// Best-effort team list (non-fatal).
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/torrent/teamList", nil)
	if err == nil {
		for k, v := range headers {
			req2.Header.Set(k, v)
		}
		if r2, err := c.Do(req2); err == nil {
			b2, _ := readLimited(r2.Body, maxResponseBody)
			r2.Body.Close()
			var data map[string]any
			if json.Unmarshal(b2, &data) == nil {
				if teams := parseOptionsList(data["data"]); len(teams) > 0 {
					if res.Codecs == nil {
						res.Codecs = map[string][]Option{}
					}
					res.Codecs["team"] = teams
				}
			}
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Result builders
// ---------------------------------------------------------------------------

func nexusphpResult(baseURL string, sections map[string]any) ProbeResult {
	res := ProbeResult{Type: TypeNexusPHPAPI, BaseURL: baseURL, Auth: "bearer", Sections: sections}
	if cats, ok := sections["categories"]; ok {
		res.Categories = walkCategoriesMap(cats)
	} else {
		res.Categories = walkCategoriesMap(sections)
	}
	if tags, ok := sections["tags"]; ok {
		res.Tags = parseOptionsList(tags)
	}
	res.Codecs = map[string][]Option{}
	for k, v := range sections {
		if strings.HasSuffix(k, "_list") {
			kind := canonicalDimKind(strings.TrimSuffix(k, "_list"))
			if opts := parseOptionsList(v); len(opts) > 0 {
				res.Codecs[kind] = opts
			}
		}
	}
	if len(res.Codecs) == 0 {
		res.Codecs = nil
	}
	return res
}

func classicResult(baseURL, body string) ProbeResult {
	res := ProbeResult{Type: TypeNexusPHPClassic, BaseURL: baseURL, Auth: "cookie"}
	if opts := parseSelectOptions(body, "type"); len(opts) > 0 {
		res.Categories = map[string]int{}
		for _, o := range opts {
			res.Categories[normToken(o.Name)] = o.ID
		}
	}
	if tags := parseCheckboxTags(body); len(tags) > 0 {
		res.Tags = tags
	}
	return res
}

func mteamResult(baseURL, body string) ProbeResult {
	res := ProbeResult{Type: TypeMTeam, BaseURL: baseURL, Auth: "x-api-key"}
	var data map[string]any
	if json.Unmarshal([]byte(body), &data) == nil {
		res.Categories = parseMTeamCategories(data["data"])
	}
	return res
}

// ---------------------------------------------------------------------------
// Enum parsing
// ---------------------------------------------------------------------------

// walkCategoriesMap recursively flattens {id,name}/{id,label} objects into a
// {name: id} map, tolerating nested "children"/list structures.
func walkCategoriesMap(v any) map[string]int {
	out := map[string]int{}
	walkCategories(v, out)
	return out
}

func walkCategories(v any, out map[string]int) {
	switch items := v.(type) {
	case map[string]any:
		if id, ok := toID(items["id"]); ok {
			if nm := firstName(items, "name", "label"); nm != "" {
				out[nm] = id
			}
		}
		for _, val := range items {
			walkCategories(val, out)
		}
	case []any:
		for _, it := range items {
			walkCategories(it, out)
		}
	}
}

// parseOptionsList parses a [{id,name}] list (or an object wrapped under
// "list"/"categories") into Option values.
func parseOptionsList(v any) []Option {
	var out []Option
	rows := unwrapList(v)
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id, ok := toID(m["id"])
		if !ok {
			continue
		}
		if nm := firstName(m, "name", "label"); nm != "" {
			out = append(out, Option{ID: id, Name: nm})
		}
	}
	return out
}

func unwrapList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		for _, k := range []string{"list", "categories", "data", "tags"} {
			if l, ok := t[k].([]any); ok {
				return l
			}
		}
	}
	return nil
}

// parseMTeamCategories maps every language variant of a category name to the
// same id (nameChs / nameCht / nameEng / name / label).
func parseMTeamCategories(v any) map[string]int {
	out := map[string]int{}
	for _, r := range unwrapList(v) {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id, ok := toID(m["id"])
		if !ok {
			continue
		}
		for _, key := range []string{"nameChs", "nameCht", "nameEng", "name", "label"} {
			if nm, ok := m[key].(string); ok && nm != "" {
				out[nm] = id
			}
		}
	}
	return out
}

func firstName(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func toID(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// HTML form parsing (classic sites)
// ---------------------------------------------------------------------------

var inputTagRe = regexp.MustCompile(`(?is)<input\b[^>]*>`)
var attrRe = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:.-]*)\s*=\s*["']([^"']*)["']`)
var selectTagRe = regexp.MustCompile(`(?is)<select\b[^>]*name\s*=\s*["']([^"']+)["'][^>]*>(.*?)</select>`)
var optionTagRe = regexp.MustCompile(`(?is)<option\b[^>]*value\s*=\s*["']([^"']*)["'][^>]*>(.*?)</option>`)
var anyTagRe = regexp.MustCompile(`(?is)<[^>]*>`)
var labelTagRe = regexp.MustCompile(`(?is)<label\b[^>]*>(.*?)</label>`)

func parseAttrs(tag string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		out[strings.ToLower(m[1])] = m[2]
	}
	return out
}

// parseHiddenInputs extracts {name: value} for all <input type="hidden"> tags.
func parseHiddenInputs(htmlSrc string) map[string]string {
	out := map[string]string{}
	for _, tag := range inputTagRe.FindAllString(htmlSrc, -1) {
		attrs := parseAttrs(tag)
		if typ := attrs["type"]; typ != "" && !strings.EqualFold(typ, "hidden") {
			continue
		}
		name := attrs["name"]
		if name == "" {
			continue
		}
		out[name] = attrs["value"]
	}
	return out
}

// parseSelectOptions extracts the options of the <select name=...> element.
func parseSelectOptions(htmlSrc, name string) []Option {
	var out []Option
	for _, m := range selectTagRe.FindAllStringSubmatch(htmlSrc, -1) {
		if m[1] != name {
			continue
		}
		for _, o := range optionTagRe.FindAllStringSubmatch(m[2], -1) {
			id, err := strconv.Atoi(strings.TrimSpace(o[1]))
			if err != nil {
				continue
			}
			label := stripTags(o[2])
			if label == "" {
				continue
			}
			out = append(out, Option{ID: id, Name: label})
		}
	}
	return out
}

// parseCheckboxTags extracts tag checkboxes (name="tags[N][]") with their
// numeric value and the adjacent label text.
func parseCheckboxTags(htmlSrc string) []Option {
	var out []Option
	for _, loc := range inputTagRe.FindAllStringIndex(htmlSrc, -1) {
		tag := htmlSrc[loc[0]:loc[1]]
		attrs := parseAttrs(tag)
		if typ := attrs["type"]; !strings.EqualFold(typ, "checkbox") {
			continue
		}
		if name := attrs["name"]; !strings.HasPrefix(name, "tags[") {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(attrs["value"]))
		if err != nil {
			continue
		}
		out = append(out, Option{ID: id, Name: labelAfter(htmlSrc, loc[1])})
	}
	return out
}

func labelAfter(htmlSrc string, pos int) string {
	rest := htmlSrc[pos:]
	if m := labelTagRe.FindStringSubmatch(rest); m != nil {
		if s := stripTags(m[1]); s != "" {
			return s
		}
	}
	end := strings.IndexAny(rest, "<\n")
	if end < 0 {
		end = len(rest)
	}
	return strings.TrimSpace(rest[:end])
}

func stripTags(s string) string {
	return strings.TrimSpace(html.UnescapeString(anyTagRe.ReplaceAllString(s, "")))
}
