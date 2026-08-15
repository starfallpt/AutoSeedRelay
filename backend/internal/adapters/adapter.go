package adapters

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/autoseedrelay/relay/internal/bencode"
	"github.com/autoseedrelay/relay/internal/parser"
)

// Adapter is the target-site upload abstraction. Implementations are
// architecture-specific (NexusPHP API / classic form / M-Team) and fully
// configured by SiteConfig; they carry no per-site hard-coded values.
type Adapter interface {
	// Name returns the configured site name.
	Name() string
	// Type returns the adapter type (one of the Type* constants).
	Type() string
	// Announce returns the rendered target announce URL (for torrent cleaning).
	Announce() string
	// Probe discovers the site's live enums (categories / tags / codecs). It is
	// read-only and used by the config wizard and dimension overrides.
	Probe(ctx context.Context) (ProbeResult, error)
	// Publish uploads a cleaned torrent to the target site.
	Publish(ctx context.Context, tor *parser.ParsedTorrent, p PublishParams) (PublishResult, error)
}

// PublishParams carries everything the relay pipeline derived for this
// torrent (produced by titler / descr / the source detail extractor). The
// adapter maps these to the target site's concrete form field names and
// value encodings.
type PublishParams struct {
	// Title is the final target-site title (already normalized upstream).
	Title string
	// SubTitle is the small description / subtitle line.
	SubTitle string
	// Description is the HTML description body.
	Description string
	// Category is the requested category: a numeric string (passed through)
	// or a name resolved via CategoryOverrides / the probed category map.
	Category string
	// Tags are source tag names, mapped through TagsMap per architecture.
	Tags []string
	// Labels are M-Team preset labels (objective attributes like language).
	Labels []string
	// IMDb is the IMDb id including the "tt" prefix (e.g. "tt1234567").
	IMDb string
	// Douban is the douban subject id (digits).
	Douban string
	// Dimensions maps a dimension kind to a canonical token, e.g.
	// {"standard":"2160","codec":"HEVC","audiocodec":"DDP"}. Kinds follow
	// titler.StandardKeys; values are the canonical tokens documented there.
	Dimensions map[string]string
	// Team is the production group name (resolved to an ID when overridden).
	Team string
	// MediaInfo is the full MediaInfo text block (optional).
	MediaInfo string
	// Countries is the list of countries/regions (M-Team).
	Countries []string
	// Anonymous requests an anonymous publish (M-Team requires an explicit bool).
	Anonymous bool
	// Uplver is the optional uploader-version flag (NexusPHP API).
	Uplver string
}

// PublishResult is the outcome of a Publish call.
type PublishResult struct {
	// OK is true when the site accepted the upload and TargetID is set.
	OK bool
	// TargetID is the target site's torrent id, 0 when unknown.
	TargetID int64
	// TestMode is true when the publish was a no-op (SiteConfig.TestMode).
	TestMode bool
	// Detail is a human-readable summary of the outcome.
	Detail string
}

// Option is one enumerated value returned by Probe (a tag or a codec).
type Option struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ---------------------------------------------------------------------------
// Shared request helpers
// ---------------------------------------------------------------------------

// newHTTPClient builds a client with the given timeout. Redirects are NOT
// followed so adapters can read 3xx Location headers (classic success /
// login-redirect detection).
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// postMultipart posts a multipart/form-data body. fields maps form names to
// values; string / int / int64 / bool / []string / []any values are supported
// (slices become repeated same-name fields). fileField is the form name for
// the torrent, fileName its filename, and fileBytes its contents. convert
// stringifies scalar values per architecture.
func postMultipart(ctx context.Context, client *http.Client, url string, headers map[string]string, fields map[string]any, fileField, fileName string, fileBytes []byte, convert func(any) string) (*http.Response, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := fields[k]
		if v == nil {
			continue
		}
		switch val := v.(type) {
		case []string:
			for _, item := range val {
				if item != "" {
					_ = w.WriteField(k, item)
				}
			}
		case []any:
			for _, item := range val {
				if item == nil {
					continue
				}
				_ = w.WriteField(k, convert(item))
			}
		default:
			_ = w.WriteField(k, convert(v))
		}
	}

	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, fileField, fileName))
	hdr.Set("Content-Type", "application/x-bittorrent")
	fw, err := w.CreatePart(hdr)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(fileBytes); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// readBody reads and closes the response body.
func readBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// encodeTorrent serializes a ParsedTorrent's raw dict for upload. The caller
// must have already cleaned it (parser.CleanTorrentForTarget).
func encodeTorrent(tor *parser.ParsedTorrent) ([]byte, error) {
	if tor == nil || tor.RawDict == nil {
		return nil, fmt.Errorf("adapters: nil torrent")
	}
	return bencode.Encode(tor.RawDict)
}

// torrentFilename derives a safe .torrent filename from the torrent name.
func torrentFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		name = "torrent"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".torrent") {
		name += ".torrent"
	}
	return name
}

// ---------------------------------------------------------------------------
// Category resolution (config-driven, no hard-coded site tables)
// ---------------------------------------------------------------------------

var imdbRe = regexp.MustCompile(`tt\d{6,}`)

// ExtractIMDB pulls the first tt id from candidate values.
func ExtractIMDB(candidates ...any) string {
	for _, c := range candidates {
		if c == nil {
			continue
		}
		s := fmt.Sprintf("%v", c)
		if s == "" {
			continue
		}
		if m := imdbRe.FindString(s); m != "" {
			return m
		}
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolveCategoryID resolves a category key to a numeric id. Resolution order:
// numeric passthrough -> CategoryOverrides -> probed site categories ->
// fallback. Returns (id, false) when nothing resolves.
func resolveCategoryID(catKey string, overrides, probed map[string]int, fallback *int) (int, bool) {
	trimmed := strings.TrimLeft(strings.TrimSpace(catKey), "-")
	if trimmed != "" && isDigits(trimmed) {
		if n, err := strconv.Atoi(trimmed); err == nil {
			return n, true
		}
	}
	if k := normToken(catKey); k != "" {
		if id, ok := overrides[k]; ok {
			return id, true
		}
		if id, ok := probed[k]; ok {
			return id, true
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Dimension resolution (config-driven)
// ---------------------------------------------------------------------------

// resolveDimID resolves a dimension token to a numeric id via
// DimensionOverrides (or numeric passthrough). It returns ok=false when the
// token cannot be represented as an id — API adapters then omit the field
// rather than send a value the site's integer taxonomy would reject.
func resolveDimID(overrides map[string]map[string]int, kind, token string) (int, bool) {
	t := normToken(token)
	if t == "" {
		return 0, false
	}
	for _, k := range dimKindAliases(kind) {
		if table := overrides[k]; table != nil {
			if id, ok := table[t]; ok {
				return id, true
			}
		}
	}
	if isDigits(t) {
		if n, err := strconv.Atoi(t); err == nil {
			return n, true
		}
	}
	return 0, false
}

// dimensionToken returns the canonical token for a kind (string form, for the
// classic adapter which submits readable values).
func dimensionToken(dims map[string]string, kinds ...string) string {
	for _, k := range kinds {
		if v, ok := dims[k]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Value converters (per-architecture scalar encoding)
// ---------------------------------------------------------------------------

// pythonStr mirrors Python's str(): booleans become "True"/"False".
func pythonStr(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "True"
		}
		return "False"
	}
	return fmt.Sprintf("%v", v)
}

// classicConvert encodes booleans as "yes"/"" and joins slices with commas.
func classicConvert(v any) string {
	switch b := v.(type) {
	case bool:
		if b {
			return "yes"
		}
		return ""
	case []any:
		return joinAny(b)
	case []string:
		return strings.Join(b, ",")
	default:
		return fmt.Sprintf("%v", v)
	}
}

// mteamConvert encodes booleans as lowercase "true"/"false" and joins slices.
func mteamConvert(v any) string {
	switch b := v.(type) {
	case bool:
		if b {
			return "true"
		}
		return "false"
	case []any:
		return joinAny(b)
	case []string:
		return strings.Join(b, ",")
	default:
		return fmt.Sprintf("%v", v)
	}
}

func joinAny(items []any) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%v", it))
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// Shared adapter base
// ---------------------------------------------------------------------------

// base holds the fields every adapter needs and the shared HTTP helpers.
type base struct {
	cfg    SiteConfig
	client *http.Client
	// probed is the live category map filled by Probe; merged under
	// cfg.CategoryOverrides at publish time.
	probed map[string]int
}

func newBase(cfg SiteConfig) base {
	cfg.Normalize()
	b := base{cfg: cfg, client: newHTTPClient(cfg.Timeout)}
	if cfg.CategoryOverrides != nil {
		b.probed = map[string]int{}
	}
	return b
}

func (b *base) Name() string     { return b.cfg.Name }
func (b *base) Type() string     { return b.cfg.Type }
func (b *base) Announce() string { return BuildAnnounce(b.cfg) }

// baseHeaders returns the shared request headers plus the caller's extras.
func baseHeaders(extra map[string]string) map[string]string {
	h := map[string]string{"User-Agent": "AutoSeedRelay/0.2 (+relay script)"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// allCategories merges config overrides over the probed cache (config wins).
func (b *base) allCategories() map[string]int {
	out := map[string]int{}
	for k, v := range b.probed {
		out[k] = v
	}
	for k, v := range b.cfg.CategoryOverrides {
		out[k] = v
	}
	return out
}

// resolveCategory resolves p.Category (with the adapter's own categories).
func (b *base) resolveCategory(p PublishParams) (int, error) {
	id, ok := resolveCategoryID(p.Category, b.cfg.CategoryOverrides, b.probed, b.cfg.FallbackCategory)
	if !ok {
		return 0, newAdapterError(ErrCategoryMismatch, 0,
			fmt.Sprintf("cannot resolve category %q to a target id (configure category_overrides or fallback_category)", p.Category), "")
	}
	return id, nil
}

// mapTags maps source tag names through TagsMap; unmapped names are dropped
// (config-driven, see docs/archive/TAG-MAPPING.md).
func mapTags(tags []string, tagsMap map[string]string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len(tagsMap) == 0 {
			// No mapping configured: pass names through unchanged.
			out = append(out, t)
			continue
		}
		if v, ok := tagsMap[t]; ok && v != "" {
			out = append(out, v)
		} else if v, ok := tagsMap[normToken(t)]; ok && v != "" {
			out = append(out, v)
		}
		// A mapping table exists but has no entry: drop the tag.
	}
	return out
}
