package checker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testBootstrapBody = `{
  "version": "1.0",
  "services": [
    [["app", "dev"], ["http://rdap.example.test/", "https://rdap.example.test/"]],
    [["COM"], ["https://rdap.verisign.example.test/com/v1/"]],
    [["broken"]]
  ]
}`

// fakeIANA serves a bootstrap file and records what it was asked.
type fakeIANA struct {
	srv  *httptest.Server
	hits atomic.Int32

	mu          sync.Mutex
	status      int    // response status; 0 means 200
	etag        string // sent with 200 responses; a matching If-None-Match gets a 304
	ifNoneMatch []string
}

func newFakeIANA(t *testing.T) *fakeIANA {
	t.Helper()
	f := &fakeIANA{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		status, etag := f.status, f.etag
		f.ifNoneMatch = append(f.ifNoneMatch, r.Header.Get("If-None-Match"))
		f.mu.Unlock()

		switch {
		case status != 0:
			w.WriteHeader(status)
		case etag != "" && r.Header.Get("If-None-Match") == etag:
			w.WriteHeader(http.StatusNotModified)
		default:
			if etag != "" {
				w.Header().Set("ETag", etag)
			}
			w.Write([]byte(testBootstrapBody))
		}
	}))
	t.Cleanup(f.srv.Close)

	oldURL := rdapBootstrapURL
	rdapBootstrapURL = f.srv.URL
	t.Cleanup(func() { rdapBootstrapURL = oldURL })
	return f
}

func (f *fakeIANA) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeIANA) lastIfNoneMatch() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ifNoneMatch) == 0 {
		return ""
	}
	return f.ifNoneMatch[len(f.ifNoneMatch)-1]
}

// withBuiltin replaces the built-in snapshot for one test.
func withBuiltin(t *testing.T, data string) {
	t.Helper()
	old := rdapBootstrapBuiltin
	rdapBootstrapBuiltin = []byte(data)
	t.Cleanup(func() { rdapBootstrapBuiltin = old })
}

// testClock is a manually advanced clock.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBootstrap(dir string, clock *testClock, logger *slog.Logger) *rdapBootstrap {
	b := newRDAPBootstrap(dir, true, logger) // the fake server is on loopback
	b.now = clock.now
	b.startedAt = clock.now()
	return b
}

func mustLookup(t *testing.T, b *rdapBootstrap, tld string) []string {
	t.Helper()
	urls, err := b.baseURLs(context.Background(), tld)
	if err != nil {
		t.Fatalf("baseURLs(%q): %v", tld, err)
	}
	return urls
}

func expectHits(t *testing.T, f *fakeIANA, want int32) {
	t.Helper()
	if got := f.hits.Load(); got != want {
		t.Fatalf("bootstrap fetched %d times, want %d", got, want)
	}
}

func TestRDAPBootstrapFetchesWhenNoCopy(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	b := newTestBootstrap("", &testClock{t: time.Now()}, nil)

	got := mustLookup(t, b, "app")
	want := []string{"https://rdap.example.test/", "http://rdap.example.test/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("base URLs = %v, want %v (HTTPS first)", got, want)
	}
	for _, tld := range []string{".app", "APP", "com", ".COM"} {
		mustLookup(t, b, tld)
	}
	if _, err := b.baseURLs(context.Background(), "invalid"); !errors.Is(err, errNoRDAP) {
		t.Errorf("unknown TLD: err = %v, want errNoRDAP", err)
	}
	if _, err := b.baseURLs(context.Background(), "broken"); !errors.Is(err, errNoRDAP) {
		t.Errorf("TLD listed without URLs: err = %v, want errNoRDAP", err)
	}
	expectHits(t, f, 1)
}

func TestRDAPBootstrapConcurrentLookupsFetchOnce(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	b := newTestBootstrap("", &testClock{t: time.Now()}, nil)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.baseURLs(context.Background(), "app"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	expectHits(t, f, 1)
}

func TestRDAPBootstrapRefreshesAfterMaxAge(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	clock := &testClock{t: time.Now()}
	b := newTestBootstrap("", clock, nil)

	mustLookup(t, b, "app")
	clock.advance(rdapBootstrapMaxAge - time.Minute)
	mustLookup(t, b, "app")
	expectHits(t, f, 1)

	clock.advance(time.Minute)
	mustLookup(t, b, "app")
	expectHits(t, f, 2)
}

func TestRDAPBootstrapNotModifiedKeepsTable(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	f.etag = `"v1"`
	clock := &testClock{t: time.Now()}
	b := newTestBootstrap("", clock, nil)

	mustLookup(t, b, "app")
	clock.advance(rdapBootstrapMaxAge)
	mustLookup(t, b, "app") // conditional refresh, answered with 304
	expectHits(t, f, 2)
	if got := f.lastIfNoneMatch(); got != `"v1"` {
		t.Errorf("If-None-Match = %q, want %q", got, `"v1"`)
	}

	// The 304 restarted the 24h clock.
	clock.advance(rdapBootstrapMaxAge - time.Minute)
	mustLookup(t, b, "app")
	expectHits(t, f, 2)
}

func TestRDAPBootstrapFailedRefreshServesOldCopyAndWaits(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	clock := &testClock{t: time.Now()}
	b := newTestBootstrap("", clock, nil)

	mustLookup(t, b, "app")
	f.setStatus(http.StatusInternalServerError)
	clock.advance(rdapBootstrapMaxAge)

	mustLookup(t, b, "app") // refresh fails; the old copy is served
	expectHits(t, f, 2)
	clock.advance(rdapBootstrapRetryAfter - time.Second)
	mustLookup(t, b, "app")
	expectHits(t, f, 2)

	f.setStatus(0)
	clock.advance(time.Second)
	mustLookup(t, b, "app")
	expectHits(t, f, 3)
}

func TestRDAPBootstrapNoCopyAndFetchFails(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	f.setStatus(http.StatusServiceUnavailable)
	b := newTestBootstrap("", &testClock{t: time.Now()}, nil)

	if _, err := b.baseURLs(context.Background(), "app"); !errors.Is(err, errRDAPBootstrapUnavailable) {
		t.Errorf("err = %v, want errRDAPBootstrapUnavailable", err)
	}
	if _, err := b.baseURLs(context.Background(), "app"); !errors.Is(err, errRDAPBootstrapUnavailable) {
		t.Errorf("err = %v, want errRDAPBootstrapUnavailable", err)
	}
	expectHits(t, f, 1) // the second lookup is inside the retry wait
}

func TestRDAPBootstrapRestartUsesSavedCopy(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	dir := t.TempDir()
	clock := &testClock{t: time.Now()}

	mustLookup(t, newTestBootstrap(dir, clock, nil), "app")
	expectHits(t, f, 1)

	saved, err := os.ReadFile(filepath.Join(dir, rdapBootstrapDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != testBootstrapBody {
		t.Error("saved file differs from the file as served")
	}

	// A new process an hour later reads the saved copy and does not fetch.
	clock.advance(time.Hour)
	restarted := newTestBootstrap(dir, clock, nil)
	if got := mustLookup(t, restarted, "dev"); got[0] != "https://rdap.example.test/" {
		t.Errorf("saved copy: base URLs = %v", got)
	}
	expectHits(t, f, 1)

	// Its age carries over: 24h after the original fetch it refreshes.
	clock.advance(rdapBootstrapMaxAge - time.Hour)
	mustLookup(t, restarted, "dev")
	expectHits(t, f, 2)
}

func TestRDAPBootstrapSavedFailureBlocksRetryAfterRestart(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	f.setStatus(http.StatusInternalServerError)
	dir := t.TempDir()
	clock := &testClock{t: time.Now()}

	newTestBootstrap(dir, clock, nil).baseURLs(context.Background(), "app")
	expectHits(t, f, 1)

	// Crash and restart a second later: the saved failure time still applies.
	clock.advance(time.Second)
	newTestBootstrap(dir, clock, nil).baseURLs(context.Background(), "app")
	expectHits(t, f, 1)

	clock.advance(rdapBootstrapRetryAfter)
	f.setStatus(0)
	mustLookup(t, newTestBootstrap(dir, clock, nil), "app")
	expectHits(t, f, 2)
}

func TestRDAPBootstrapCorruptSavedCopyIsRefetched(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	f.etag = `"v1"`
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, rdapBootstrapDataFile), []byte("{not json"), 0o600)
	os.WriteFile(filepath.Join(dir, rdapBootstrapMetaFile),
		[]byte(`{"fetched_at":"`+time.Now().Format(time.RFC3339)+`","etag":"\"v1\""}`), 0o600)

	var logs bytes.Buffer
	b := newTestBootstrap(dir, &testClock{t: time.Now()}, slog.New(slog.NewTextHandler(&logs, nil)))
	mustLookup(t, b, "app")
	expectHits(t, f, 1)

	// The validators described the unreadable copy, so the fetch must not be
	// conditional: a 304 would leave nothing to serve.
	if got := f.lastIfNoneMatch(); got != "" {
		t.Errorf("If-None-Match = %q, want none", got)
	}
	if !strings.Contains(logs.String(), "ignoring unreadable RDAP bootstrap cache") {
		t.Errorf("corrupt file was not logged: %s", logs.String())
	}
	if saved, _ := os.ReadFile(filepath.Join(dir, rdapBootstrapDataFile)); string(saved) != testBootstrapBody {
		t.Error("corrupt file was not replaced")
	}
}

func TestRDAPBootstrapUnwritableDirKeepsWorkingAndWarnsOnce(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t)
	dir := filepath.Join(t.TempDir(), "missing")
	clock := &testClock{t: time.Now()}
	var logs bytes.Buffer
	b := newTestBootstrap(dir, clock, slog.New(slog.NewTextHandler(&logs, nil)))

	mustLookup(t, b, "app")
	clock.advance(rdapBootstrapMaxAge)
	mustLookup(t, b, "app")
	expectHits(t, f, 2)

	if n := strings.Count(logs.String(), "could not save RDAP bootstrap cache"); n != 1 {
		t.Errorf("save failure logged %d times, want 1:\n%s", n, logs.String())
	}
}

func TestRDAPBootstrapStartupDelayWithBuiltinCopy(t *testing.T) {
	withBuiltin(t, `{"services":[[["app"],["https://builtin.example.test/"]]]}`)
	f := newFakeIANA(t)
	clock := &testClock{t: time.Now()}
	b := newTestBootstrap("", clock, nil)

	if got := mustLookup(t, b, "app"); got[0] != "https://builtin.example.test/" {
		t.Errorf("base URLs = %v, want the built-in copy", got)
	}
	clock.advance(rdapBootstrapStartupDelay - time.Second)
	mustLookup(t, b, "app")
	expectHits(t, f, 0)

	clock.advance(time.Second)
	if got := mustLookup(t, b, "app"); got[0] != "https://rdap.example.test/" {
		t.Errorf("base URLs = %v, want the fetched copy", got)
	}
	expectHits(t, f, 1)
}

func TestRDAPBootstrapSavedCopyPreferredOverBuiltin(t *testing.T) {
	withBuiltin(t, `{"services":[[["app"],["https://builtin.example.test/"]]]}`)
	f := newFakeIANA(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, rdapBootstrapDataFile), []byte(testBootstrapBody), 0o600)
	os.WriteFile(filepath.Join(dir, rdapBootstrapMetaFile),
		[]byte(`{"fetched_at":"`+time.Now().Format(time.RFC3339)+`"}`), 0o600)

	b := newTestBootstrap(dir, &testClock{t: time.Now()}, nil)
	if got := mustLookup(t, b, "app"); got[0] != "https://rdap.example.test/" {
		t.Errorf("base URLs = %v, want the saved copy", got)
	}
	expectHits(t, f, 0)
}

func TestRDAPBootstrapRejectsBadResponses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":    "<html>",
		"no services": `{"services":[]}`,
		"too large":   strings.Repeat(" ", rdapBootstrapMaxBody+1),
	} {
		t.Run(name, func(t *testing.T) {
			withBuiltin(t, `{"services":[]}`)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(body))
			}))
			defer srv.Close()
			old := rdapBootstrapURL
			rdapBootstrapURL = srv.URL
			defer func() { rdapBootstrapURL = old }()

			b := newTestBootstrap("", &testClock{t: time.Now()}, nil)
			if _, err := b.baseURLs(context.Background(), "app"); !errors.Is(err, errRDAPBootstrapUnavailable) {
				t.Errorf("err = %v, want errRDAPBootstrapUnavailable", err)
			}
		})
	}
}

func TestRDAPBootstrapBlocksPrivateAddresses(t *testing.T) {
	withBuiltin(t, `{"services":[]}`)
	f := newFakeIANA(t) // listens on loopback
	b := newRDAPBootstrap("", false, nil)

	if _, err := b.baseURLs(context.Background(), "app"); !errors.Is(err, errRDAPBootstrapUnavailable) {
		t.Errorf("err = %v, want errRDAPBootstrapUnavailable", err)
	}
	expectHits(t, f, 0)
}

func TestDefaultRegistryDoesNoBootstrapIO(t *testing.T) {
	f := newFakeIANA(t)
	dir := t.TempDir()

	DefaultRegistry(nil, true, WithCacheDir(dir))

	expectHits(t, f, 0)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("creating the registry wrote %d files to the cache directory", len(entries))
	}
}

func TestBuiltinRDAPBootstrapParses(t *testing.T) {
	if _, err := parseRDAPBootstrap(rdapBootstrapBuiltin); err != nil {
		t.Fatalf("built-in snapshot: %v", err)
	}
}
