package adapters

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/autoseedrelay/relay/internal/parser"
)

// NexusPHPClassic adapts a legacy NexusPHP form site: GET upload.php (hidden
// fields + category options) then POST takeupload.php with the login cookie.
type NexusPHPClassic struct {
	base
}

func newNexusPHPClassic(cfg SiteConfig) Adapter {
	cfg.Type = TypeNexusPHPClassic
	return &NexusPHPClassic{base: newBase(cfg)}
}

func (n *NexusPHPClassic) uploadURL() string {
	return strings.TrimRight(n.cfg.BaseURL, "/") + "/" + strings.TrimLeft(n.cfg.UploadPath, "/")
}

func (n *NexusPHPClassic) uploadPageURL() string {
	return strings.TrimRight(n.cfg.BaseURL, "/") + "/upload.php"
}

func (n *NexusPHPClassic) headers() map[string]string {
	h := baseHeaders(nil)
	if n.cfg.Cookie != "" {
		h["Cookie"] = n.cfg.Cookie
	}
	return h
}

// Probe implements Adapter.Probe for the classic form architecture.
func (n *NexusPHPClassic) Probe(ctx context.Context) (ProbeResult, error) {
	return probeClassic(ctx, n.client, n.cfg.BaseURL, n.headers())
}

var detailsIDRe = regexp.MustCompile(`details\.php\?id=(\d+)`)

// Publish uploads a cleaned torrent through the classic form flow.
func (n *NexusPHPClassic) Publish(ctx context.Context, tor *parser.ParsedTorrent, p PublishParams) (PublishResult, error) {
	url := n.uploadURL()
	if n.cfg.TestMode {
		return PublishResult{TestMode: true, Detail: "test mode: would POST " + url},
			newAdapterError(ErrTestMode, 0, "test mode: publish skipped (would POST "+url+")", "")
	}
	if n.cfg.Cookie == "" {
		return PublishResult{}, newAdapterError(ErrAuthExpired, 0,
			"classic form upload requires a login cookie (SiteConfig.Cookie)", "")
	}

	category, err := n.resolveCategory(p)
	if err != nil {
		return PublishResult{}, err
	}

	descr := p.Description
	if tags := nonEmpty(p.Tags); len(tags) > 0 {
		descr = appendLine(descr, "[标签:"+strings.Join(tags, ",")+"]")
	}
	var params []string
	if v := dimensionToken(p.Dimensions, "standard"); v != "" {
		params = append(params, v)
	}
	if v := dimensionToken(p.Dimensions, "video_codec", "codec"); v != "" {
		params = append(params, v)
	}
	if v := dimensionToken(p.Dimensions, "audio_codec", "audiocodec"); v != "" {
		params = append(params, v)
	}
	if p.Team != "" {
		params = append(params, p.Team)
	}
	if len(params) > 0 {
		descr = appendLine(descr, "[参数:"+strings.Join(params, ",")+"]")
	}

	fields := map[string]any{
		"name":  p.Title,
		"descr": descr,
		"type":  category,
	}
	if p.SubTitle != "" {
		fields["small_descr"] = p.SubTitle
	}
	if imdb := ExtractIMDB(p.IMDb); imdb != "" {
		fields["url"] = strings.TrimPrefix(imdb, "tt")
	}
	if p.MediaInfo != "" {
		fields["technical_info"] = p.MediaInfo
	}
	if p.Uplver != "" {
		fields["uplver"] = "yes"
	}

	// Step 1: fetch the upload form to collect hidden fields (CSRF tokens,
	// session ids, ...) and refresh the category cache. Failure is non-fatal
	// except for an explicit login redirect (auth expired).
	hidden, authErr := n.fetchHiddenFields(ctx)
	if authErr != nil {
		return PublishResult{}, authErr
	}
	for k, v := range hidden {
		if _, exists := fields[k]; !exists {
			fields[k] = v
		}
	}

	fileBytes, err := encodeTorrent(tor)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, 0, "encode torrent: "+err.Error(), "")
	}
	resp, err := postMultipart(ctx, n.client, url, n.headers(), fields, "file", torrentFilename(tor.Name), fileBytes, classicConvert)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, 0, "upload request failed: "+err.Error(), err.Error())
	}
	body, err := readBody(resp)
	if err != nil {
		return PublishResult{}, newAdapterError(nil, resp.StatusCode, "read response: "+err.Error(), "")
	}

	if isClassicLoginRedirect(resp, body) {
		return PublishResult{}, newAdapterError(ErrAuthExpired, resp.StatusCode,
			"classic upload redirected to login (cookie expired)", body)
	}

	loc := resp.Header.Get("Location")
	m := detailsIDRe.FindStringSubmatch(loc)
	if m == nil {
		m = detailsIDRe.FindStringSubmatch(body)
	}
	if m != nil {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		return PublishResult{OK: true, TargetID: id, Detail: fmt.Sprintf("classic upload ok id=%d", id)}, nil
	}

	if isDuplicateBody(body, resp.StatusCode, classicExistingHints) {
		return PublishResult{}, newAdapterError(ErrDuplicate, resp.StatusCode,
			"classic site reports the torrent already exists", body)
	}
	return PublishResult{}, newAdapterError(nil, resp.StatusCode,
		fmt.Sprintf("classic upload failed: HTTP %d", resp.StatusCode), body)
}

// fetchHiddenFields GETs upload.php and returns its hidden inputs. A login
// redirect is reported as ErrAuthExpired; any other failure returns no fields.
func (n *NexusPHPClassic) fetchHiddenFields(ctx context.Context) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.uploadPageURL(), nil)
	if err != nil {
		return nil, nil
	}
	for k, v := range n.headers() {
		req.Header.Set(k, v)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, nil // network failure is non-fatal; proceed with direct POST
	}
	defer resp.Body.Close()

	if isClassicLoginRedirect(resp, "") {
		return nil, newAdapterError(ErrAuthExpired, resp.StatusCode,
			"classic upload page redirected to login (cookie expired)", "")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	body, err := readBody(resp)
	if err != nil {
		return nil, nil
	}
	hidden := parseHiddenInputs(body)
	// Cache the category <select> so later publishes can resolve names without
	// an explicit Probe.
	if opts := parseSelectOptions(body, "type"); len(opts) > 0 {
		if n.probed == nil {
			n.probed = map[string]int{}
		}
		for _, o := range opts {
			n.probed[normToken(o.Name)] = o.ID
		}
	}
	return hidden, nil
}

// isClassicLoginRedirect reports whether a response indicates the session
// expired and the server wants a re-login.
func isClassicLoginRedirect(resp *http.Response, body string) bool {
	loc := resp.Header.Get("Location")
	if strings.Contains(loc, "login.php") || strings.Contains(loc, "login") {
		return true
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return true
	}
	low := strings.ToLower(body)
	for _, h := range []string{"login.php", "请先登录", "未登录", "重新登录", "帐号已过期", "账号已过期"} {
		if strings.Contains(low, strings.ToLower(h)) {
			return true
		}
	}
	return false
}

var classicExistingHints = []string{
	"种子已存在",
	"already exists",
	"重复",
	"existed",
	"duplicate torrent",
}

func appendLine(descr, line string) string {
	if descr == "" {
		return line
	}
	return descr + "\n" + line
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
