package checker

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/y0f/asura/internal/safenet"
)

// The RDAP bootstrap registry (RFC 9224) maps each TLD to the base URLs of
// its registry's RDAP service. This file loads, caches, saves and refreshes
// it; the per-domain RDAP query lives elsewhere.
//
// Nothing here runs until a domain check asks for a TLD: there is no init()
// work, background goroutine or timer, so an instance without domain
// monitors never contacts IANA.

// rdapBootstrapURL is IANA's bootstrap file for domains; a variable so tests
// can point it at a local server.
var rdapBootstrapURL = "https://data.iana.org/rdap/dns.json"

const (
	// rdapBootstrapMaxAge is how long a fetched copy is used before refreshing.
	rdapBootstrapMaxAge = 24 * time.Hour
	// rdapBootstrapRetryAfter is how long to wait after a failed fetch.
	rdapBootstrapRetryAfter = 10 * time.Minute
	// rdapBootstrapStartupDelay is how long a process serving only the
	// built-in copy waits before its first fetch, so a crash loop does not
	// download the file on every restart.
	rdapBootstrapStartupDelay = 10 * time.Minute
	rdapBootstrapFetchTimeout = 10 * time.Second
	rdapBootstrapMaxBody      = 4 << 20

	rdapBootstrapDataFile = "rdap-bootstrap.json"
	rdapBootstrapMetaFile = "rdap-bootstrap.meta.json"
)

// errRDAPBootstrapUnavailable reports that no copy of the bootstrap file is
// available: none fetched, none saved, and no usable built-in copy.
var errRDAPBootstrapUnavailable = errors.New("RDAP bootstrap file unavailable")

// errNoRDAP reports that the bootstrap file lists no RDAP service for a TLD.
var errNoRDAP = errors.New("no RDAP service for TLD")

// rdapBootstrapBuiltin is the snapshot shipped in the binary, refreshed with
// `make rdap-bootstrap`. It is parsed only when no fetched or saved copy
// exists.
//
//go:embed rdap_bootstrap.json
var rdapBootstrapBuiltin []byte

// Where the copy in memory came from.
const (
	rdapSourceNone    = ""
	rdapSourceBuiltin = "builtin"
	rdapSourceSaved   = "saved"
	rdapSourceFetched = "fetched"
)

// rdapBootstrapMeta is saved next to the bootstrap file so its age, a recent
// failure and the HTTP validators survive a restart.
type rdapBootstrapMeta struct {
	FetchedAt    time.Time `json:"fetched_at,omitempty"`
	FailedAt     time.Time `json:"failed_at,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
}

// rdapBootstrap is the bootstrap cache. Its zero value is not usable; build
// one with newRDAPBootstrap.
type rdapBootstrap struct {
	cacheDir     string // empty: keep the cache in memory only
	allowPrivate bool
	logger       *slog.Logger
	now          func() time.Time
	startedAt    time.Time

	// mu is held for the whole of a lookup, fetch included, so checks that
	// arrive together cause one download.
	mu         sync.Mutex
	loaded     bool // the saved and built-in copies have been read
	services   map[string][]string
	source     string
	meta       rdapBootstrapMeta
	warnedSave bool
	warnedLoad bool
}

// newRDAPBootstrap returns a bootstrap cache that saves to cacheDir (empty
// for memory only). It does no I/O.
func newRDAPBootstrap(cacheDir string, allowPrivate bool, logger *slog.Logger) *rdapBootstrap {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &rdapBootstrap{
		cacheDir:     cacheDir,
		allowPrivate: allowPrivate,
		logger:       logger,
		now:          time.Now,
		startedAt:    time.Now(),
	}
}

// baseURLs returns the RDAP base URLs for tld (with or without the leading
// dot), loading or refreshing the bootstrap file first if needed. It returns
// errNoRDAP when the TLD has no RDAP service and errRDAPBootstrapUnavailable
// when there is no copy of the file to consult.
func (b *rdapBootstrap) baseURLs(ctx context.Context, tld string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.loaded {
		b.loadLocked()
		b.loaded = true
	}
	if b.needsRefreshLocked() {
		b.refreshLocked(ctx)
	}

	if b.services == nil {
		return nil, errRDAPBootstrapUnavailable
	}
	urls := b.services[strings.ToLower(strings.TrimPrefix(tld, "."))]
	if len(urls) == 0 {
		return nil, errNoRDAP
	}
	return append([]string(nil), urls...), nil
}

// loadLocked fills the cache from the saved copy, or failing that from the
// built-in one.
func (b *rdapBootstrap) loadLocked() {
	if b.cacheDir != "" {
		b.meta = b.readMetaLocked()
		if services, ok := b.readSavedLocked(); ok {
			b.services, b.source = services, rdapSourceSaved
			return
		}
		// The validators and fetch time describe a copy we no longer have;
		// keep only the failure time, which still applies.
		b.meta = rdapBootstrapMeta{FailedAt: b.meta.FailedAt}
	}
	if services, err := parseRDAPBootstrap(rdapBootstrapBuiltin); err == nil && len(services) > 0 {
		b.services, b.source = services, rdapSourceBuiltin
	}
}

func (b *rdapBootstrap) readMetaLocked() rdapBootstrapMeta {
	var meta rdapBootstrapMeta
	data, err := os.ReadFile(filepath.Join(b.cacheDir, rdapBootstrapMetaFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.warnLoadLocked(rdapBootstrapMetaFile, err)
		}
		return meta
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		b.warnLoadLocked(rdapBootstrapMetaFile, err)
		return rdapBootstrapMeta{}
	}
	return meta
}

func (b *rdapBootstrap) readSavedLocked() (map[string][]string, bool) {
	data, err := os.ReadFile(filepath.Join(b.cacheDir, rdapBootstrapDataFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.warnLoadLocked(rdapBootstrapDataFile, err)
		}
		return nil, false
	}
	services, err := parseRDAPBootstrap(data)
	if err == nil && len(services) == 0 {
		err = errors.New("no services")
	}
	if err != nil {
		b.warnLoadLocked(rdapBootstrapDataFile, err)
		return nil, false
	}
	return services, true
}

// needsRefreshLocked reports whether a lookup should fetch the file first.
func (b *rdapBootstrap) needsRefreshLocked() bool {
	now := b.now()
	if !b.meta.FailedAt.IsZero() && now.Sub(b.meta.FailedAt) < rdapBootstrapRetryAfter {
		return false
	}
	switch b.source {
	case rdapSourceSaved, rdapSourceFetched:
		return now.Sub(b.meta.FetchedAt) >= rdapBootstrapMaxAge
	case rdapSourceBuiltin:
		return now.Sub(b.startedAt) >= rdapBootstrapStartupDelay
	default:
		// Nothing to serve: fetch now.
		return true
	}
}

// refreshLocked fetches the bootstrap file. On failure the copy in memory, if
// any, stays in use and the failure time is recorded.
func (b *rdapBootstrap) refreshLocked(ctx context.Context) {
	now := b.now()
	conditional := b.source == rdapSourceSaved || b.source == rdapSourceFetched

	res, err := b.fetch(ctx, conditional)
	if err != nil {
		b.logger.Warn("RDAP bootstrap refresh failed", "url", rdapBootstrapURL, "error", err,
			"serving", b.source, "retry_after", rdapBootstrapRetryAfter.String())
		b.meta.FailedAt = now
		b.saveMetaLocked()
		return
	}

	b.meta.FailedAt = time.Time{}
	b.meta.FetchedAt = now
	if !res.notModified {
		b.services, b.source = res.services, rdapSourceFetched
		b.meta.ETag, b.meta.LastModified = res.etag, res.lastModified
		b.saveDataLocked(res.data)
	}
	b.saveMetaLocked()
}

// rdapFetchResult is the outcome of a successful bootstrap fetch.
type rdapFetchResult struct {
	notModified  bool   // 304: the copy we have is current
	data         []byte // the file as served, for saving
	services     map[string][]string
	etag         string
	lastModified string
}

// fetch downloads the bootstrap file with a client of its own: the file is
// shared by every monitor, so it never goes through a monitor's proxy. The
// client is dropped afterwards; a fetch happens about once a day.
func (b *rdapBootstrap) fetch(ctx context.Context, conditional bool) (*rdapFetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, rdapBootstrapFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rdapBootstrapURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if conditional {
		if b.meta.ETag != "" {
			req.Header.Set("If-None-Match", b.meta.ETag)
		}
		if b.meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", b.meta.LastModified)
		}
	}

	client := &http.Client{
		Timeout: rdapBootstrapFetchTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: rdapBootstrapFetchTimeout,
				Control: safenet.MaybeDialControl(b.allowPrivate),
			}).DialContext,
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified && conditional:
		return &rdapFetchResult{notModified: true}, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, rdapBootstrapMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(data) > rdapBootstrapMaxBody {
		return nil, fmt.Errorf("larger than %d bytes", rdapBootstrapMaxBody)
	}
	services, err := parseRDAPBootstrap(data)
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, errors.New("no services listed")
	}
	return &rdapFetchResult{
		data:         data,
		services:     services,
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

// rdapBootstrapFile is the shape of the bootstrap file (RFC 9224 §4): each
// service is a pair of [TLDs, base URLs].
type rdapBootstrapFile struct {
	Services [][][]string `json:"services"`
}

// parseRDAPBootstrap turns a bootstrap file into a TLD → base URLs table,
// HTTPS URLs first. TLDs are stored lowercase without a leading dot.
func parseRDAPBootstrap(data []byte) (map[string][]string, error) {
	var doc rdapBootstrapFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid bootstrap file: %w", err)
	}
	services := make(map[string][]string)
	for _, svc := range doc.Services {
		if len(svc) < 2 || len(svc[1]) == 0 {
			continue
		}
		urls := httpsFirst(svc[1])
		for _, tld := range svc[0] {
			if key := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(tld), ".")); key != "" {
				services[key] = urls
			}
		}
	}
	return services, nil
}

// httpsFirst returns urls with HTTPS entries before the rest, otherwise in
// their original order.
func httpsFirst(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if strings.HasPrefix(strings.ToLower(u), "https://") {
			out = append(out, u)
		}
	}
	for _, u := range urls {
		if !strings.HasPrefix(strings.ToLower(u), "https://") {
			out = append(out, u)
		}
	}
	return out
}

// saveDataLocked saves the file exactly as IANA served it.
func (b *rdapBootstrap) saveDataLocked(data []byte) {
	if b.cacheDir == "" {
		return
	}
	if err := writeFileAtomic(b.cacheDir, rdapBootstrapDataFile, data); err != nil {
		b.warnSaveLocked(rdapBootstrapDataFile, err)
	}
}

func (b *rdapBootstrap) saveMetaLocked() {
	if b.cacheDir == "" {
		return
	}
	data, err := json.Marshal(b.meta)
	if err == nil {
		err = writeFileAtomic(b.cacheDir, rdapBootstrapMetaFile, data)
	}
	if err != nil {
		b.warnSaveLocked(rdapBootstrapMetaFile, err)
	}
}

// warnSaveLocked logs the first failed save only: an unwritable directory
// would otherwise log on every refresh.
func (b *rdapBootstrap) warnSaveLocked(name string, err error) {
	if b.warnedSave {
		return
	}
	b.warnedSave = true
	b.logger.Warn("could not save RDAP bootstrap cache; keeping it in memory only",
		"file", filepath.Join(b.cacheDir, name), "error", err)
}

func (b *rdapBootstrap) warnLoadLocked(name string, err error) {
	if b.warnedLoad {
		return
	}
	b.warnedLoad = true
	b.logger.Warn("ignoring unreadable RDAP bootstrap cache",
		"file", filepath.Join(b.cacheDir, name), "error", err)
}

// writeFileAtomic writes data to dir/name through a temporary file and a
// rename, so a crash mid-write never leaves a truncated file behind.
func writeFileAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
