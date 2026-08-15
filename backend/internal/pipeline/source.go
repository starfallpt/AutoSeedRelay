package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/autoseedrelay/relay/internal/qb"
	"github.com/autoseedrelay/relay/internal/source"
	"github.com/autoseedrelay/relay/internal/store"
)

// SourceProvider performs the source-site operations the pipeline needs:
// locate the RSS item for a seed (re-fetching RSS and matching on info_hash),
// fetch the seed's detail, and download the .torrent (qB direct-pull
// preferred, direct HTTP fallback).
type SourceProvider interface {
	Locate(ctx context.Context, seed *store.Seed) (*source.RssItem, error)
	FetchDetail(ctx context.Context, torrentID int) (*source.SeedDetail, error)
	Download(ctx context.Context, item *source.RssItem, outPath string) error
}

// SourceConfig injects test overrides into a SourceProvider.
type SourceConfig struct {
	// URLChecker overrides the SSRF check applied to download/detail requests.
	// nil uses the source package's strict default (rejects loopback/private).
	URLChecker func(string) error
	// HTTPClient overrides the client used for RSS / download / detail.
	HTTPClient *http.Client
	// Backoff overrides the download retry backoff.
	Backoff source.BackoffFunc
	// FetchRSS overrides RSS retrieval (the default source.FetchRSS does strict
	// SSRF and would reject the loopback httptest servers used in tests).
	FetchRSS func(ctx context.Context, url string, client *http.Client) ([]source.RssItem, error)
}

// sourceProvider is the concrete SourceProvider built from a source row.
type sourceProvider struct {
	rssURL     string
	passkey    string
	cookie     string
	apiToken   string
	httpClient *http.Client
	urllCheck  func(string) error
	backoff    source.BackoffFunc
	qb         func(ctx context.Context) *qb.Instance
	fetchRSS   func(ctx context.Context, url string, client *http.Client) ([]source.RssItem, error)
	detail     *source.DetailFetcher
}

// NewSourceProvider builds a SourceProvider from a source row. pickQB supplies
// the qB instance used for the preferred direct-pull download (may be nil, in
// which case Download always uses the direct HTTP path).
func NewSourceProvider(src *store.Source, pickQB func(ctx context.Context) *qb.Instance, cfg SourceConfig) SourceProvider {
	fetchRSS := cfg.FetchRSS
	if fetchRSS == nil {
		fetchRSS = source.FetchRSS
	}
	return &sourceProvider{
		rssURL:     src.RSSURL,
		passkey:    src.Passkey,
		cookie:     src.Cookie,
		apiToken:   src.APIToken,
		httpClient: cfg.HTTPClient,
		urllCheck:  cfg.URLChecker,
		backoff:    cfg.Backoff,
		qb:         pickQB,
		fetchRSS:   fetchRSS,
		detail: source.NewDetailFetcher(src.BaseURL, source.DetailFetcherOptions{
			Cookie:     src.Cookie,
			APIToken:   src.APIToken,
			HTTPClient: cfg.HTTPClient,
			URLChecker: cfg.URLChecker,
		}),
	}
}

// Locate re-fetches the source RSS and returns the item whose normalized
// info_hash equals the seed's, giving the pipeline the torrent id + download
// URL it needs to fetch detail and the .torrent.
func (s *sourceProvider) Locate(ctx context.Context, seed *store.Seed) (*source.RssItem, error) {
	items, err := s.fetchRSS(ctx, s.rssURL, s.httpClient)
	if err != nil {
		return nil, fmt.Errorf("pipeline: fetch source RSS: %w", err)
	}
	want := strings.ToLower(strings.TrimSpace(seed.InfoHash))
	for i := range items {
		it := &items[i]
		if strings.EqualFold(it.Infohash(), want) {
			return it, nil
		}
		if strings.EqualFold(strings.TrimSpace(it.GUID), want) {
			return it, nil
		}
	}
	return nil, fmt.Errorf("pipeline: seed info_hash %q not found in source RSS (%d items)", seed.InfoHash, len(items))
}

// FetchDetail returns the source seed's detail.
func (s *sourceProvider) FetchDetail(ctx context.Context, torrentID int) (*source.SeedDetail, error) {
	return s.detail.FetchAllDetail(ctx, torrentID)
}

// Download writes the .torrent to outPath, trying qB direct-pull first and
// falling back to a direct HTTP download.
func (s *sourceProvider) Download(ctx context.Context, item *source.RssItem, outPath string) error {
	var qbErr error
	if s.qb != nil {
		if inst := s.qb(ctx); inst != nil {
			_, qbErr = s.client("qb", inst).DownloadTorrent(ctx, item, outPath)
			if qbErr == nil {
				return nil
			}
		}
	}
	_, directErr := s.client("direct", nil).DownloadTorrent(ctx, item, outPath)
	if directErr == nil {
		return nil
	}
	if qbErr != nil {
		return fmt.Errorf("qb direct-pull failed: %v; direct download failed: %v", qbErr, directErr)
	}
	return fmt.Errorf("direct download failed: %v", directErr)
}

// client builds a source.Client for the given download mode.
func (s *sourceProvider) client(mode string, inst *qb.Instance) *source.Client {
	return source.NewClient(s.rssURL, source.ClientOptions{
		DownloadMode: mode,
		Passkey:      s.passkey,
		Cookie:       s.cookie,
		APIToken:     s.apiToken,
		Backoff:      s.backoff,
		URLChecker:   s.urllCheck,
		HTTPClient:   s.httpClient,
		QB:           inst,
	})
}
