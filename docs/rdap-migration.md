# Domain monitor: RDAP migration plan

The `domain` monitor type finds a domain's expiry date over port-43 WHOIS, using a hardcoded
table of servers (`internal/checker/domain.go`). That works for `.com`, `.net` and `.org`, but
fails for many other TLDs (for example `.app`, which returns "WHOIS connection failed"). Since
ICANN's WHOIS sunset (January 2025), gTLD registries only have to run RDAP, and several no
longer answer on port 43.

This plan makes RDAP the primary lookup, with WHOIS kept as a fallback for TLDs that have no
RDAP service. It is delivered in steps, each reviewed on its own:

| Step | Scope | Status |
|---|---|---|
| 1 | RDAP bootstrap: loading, caching, saving to disk, expiry, refresh (§1a, §1f wiring) | in review |
| 2 | Domain name preparation and the per-domain RDAP query (§1b–§1e) | not started |
| 3 | WHOIS fixes (§3) | not started |
| 4 | Docs (§1g) | not started |

---

## 1. How the RDAP lookup works

### 1a. Bootstrap: finding the RDAP server for a TLD

**Source:** IANA's `https://data.iana.org/rdap/dns.json` (RFC 9224), which maps each TLD to one
or more RDAP base URLs. It is parsed into a TLD → base-URLs table, with HTTPS URLs first.

**When work happens:**
- Nothing runs at startup or in the background: no `init()` parsing, no goroutine, no timer.
- All bootstrap work happens inline, the first time a domain check needs it.
- An instance with no domain monitors makes no requests and holds nothing in memory.

**Where a copy comes from, in order of preference:**
1. **In memory.** One entry with one mutex, like the OAuth2 token cache in
   `internal/checker/http.go`. The lock is held during a fetch, so many checks arriving together
   cause one download.
2. **The saved file.** `rdap-bootstrap.json`, plus `rdap-bootstrap.meta.json` holding the fetch
   time, last failure time, `ETag` and `Last-Modified`.
   - It lives next to the database (the directory of `database.path`). That is the only place
     guaranteed writable and persistent in both supported deployments: the `/app/data` volume
     in Docker, and `ReadWritePaths=/var/lib/asura` under systemd.
   - Writes go to a temp file, then a rename.
   - Read or write errors are logged once, and the check carries on from memory and the
     built-in copy.
   - A corrupt file is ignored and fetched again.
3. **The built-in copy.** A snapshot of `dns.json` embedded with `go:embed`, as `web/embed.go`
   does for static assets. It is parsed only when there is no other copy, and refreshed in the
   repo with `make rdap-bootstrap`, like the public suffix list is updated with its dependency.

**Refresh rules:**
- A copy under 24h old (in memory or on disk) is used with no request.
- A refresh sends `If-None-Match` / `If-Modified-Since`. A `304` keeps the current table and
  resets the 24h clock.
- After a failed refresh, the copy we have keeps being served. No retry for 10 minutes, and the
  failure time is saved, so this holds across restarts.
- **Startup delay:** when the only copy is the built-in one, no request is made until the
  process has been up for 10 minutes. This protects IANA from crash loops (the agent, or a
  first run). If the built-in copy is empty, there is nothing to serve, so the fetch happens
  right away.

**Fetch client:**
- A new client for each fetch: safenet dialer, keep-alives off, 10s timeout. It is
  garbage-collected afterwards, as in `fetchOAuth2Token`.
- It never uses a monitor's proxy, because the bootstrap is shared by all monitors.
- The response is capped at 4 MB.
- The URL is a package variable so tests can replace it.

**Scope of a cache:** each registry builds its own cache (one per `DefaultRegistry` call), so
tests use a fresh one instead of resetting a global.

### 1b. Preparing the domain name
- Keep the existing steps: trim, lowercase, strip the trailing dot, reduce to the registrable
  domain with the public suffix list.
- New: convert internationalised (IDN) names to punycode with `golang.org/x/net/idna` (already a
  dependency). Both RDAP and WHOIS expect that form.
- The bootstrap lookup key is the domain's last label.

### 1c. The per-domain RDAP query
- `GET {base}domain/{domain}`, `Accept: application/rdap+json`.
- Each check builds its own client, following the HTTP checker:
  - safenet dialer;
  - the monitor's SOCKS5 or HTTP proxy, if set;
  - with a proxy, `validateProxyTarget` on every request, redirects included;
  - at most 5 redirects, body capped at 1 MB.
- If a TLD lists several base URLs, try the next one after a transport error or a 5xx.

### 1d. Timeouts
- The whole check (bootstrap if needed, then RDAP, then the WHOIS fallback) runs under one
  deadline equal to the monitor's timeout.
- A bootstrap refresh started inside a check uses whichever is shorter: 10s or the time the
  check has left.

### 1e. Handling the response

| Response | Result |
|---|---|
| 200 with an `expiration` event | Parse as RFC 3339, also accepting fractional seconds and a missing zone (assume UTC). Apply the warning threshold. |
| 200 with no `expiration` event | Fall back to WHOIS. If WHOIS has no date either: "registry does not publish expiry". |
| 404 | `down`: domain not registered |
| 429, 5xx or transport error, after every base URL | `down` with the RDAP error and no WHOIS fallback (decision 1) |

**WHOIS is used** only when the TLD is not in the bootstrap table, no bootstrap copy is
available at all, or the RDAP response has no expiration event.

**Result details:** `Body` holds the raw response, truncated to 8 KB. The message names the
source ("via RDAP" or "via WHOIS").

### 1f. Code layout
- **`internal/checker/rdap_bootstrap.go`:** the bootstrap cache (memory, file, built-in copy)
  and its fetch client.
- **`internal/checker/rdap_bootstrap.json`:** the built-in snapshot.
- **`internal/checker/rdap.go`** (step 2): the per-domain RDAP query and parsing.
- **`internal/checker/domain.go`:** coordinates the check (RDAP first, WHOIS fallback), the
  WHOIS fixes, and one shared function that turns an expiry date into up / degraded / down.
- **`DefaultRegistry`:** takes optional `WithCacheDir` and `WithLogger` options, passed on to
  `DomainChecker`; existing callers are unchanged.
  - The server passes the database's directory (`cmd/asura/main.go`).
  - The agent passes its new optional `-cache-dir` flag (`cmd/asura-agent/main.go`). Without
    it, the agent uses the built-in copy plus the startup delay.
- No changes to the storage layer or the schema.

### 1g. Docs
- Rename "Domain (WHOIS)" in `docs/pages/monitors.html` and in the README feature list.
- Describe RDAP first with WHOIS fallback.
- List the network needs: outbound HTTPS to `data.iana.org` and the registries' RDAP servers,
  and port 43 for the fallback.
- Document the cache file next to the database, the agent's `-cache-dir` flag, and how proxies
  behave (§4).

### 1h. Tests

**Bootstrap and cache (step 1):**
- many checks at once cause one download;
- the 24h expiry;
- a `304` keeps the table;
- serving the old copy after a failed refresh, and the 10-minute retry wait;
- after a restart, reading the saved file makes no request;
- a saved failure time blocks a retry;
- a corrupt file is ignored and fetched again;
- an unwritable directory is handled;
- no request during the startup delay with only the built-in copy;
- running with no cache directory (the agent without `-cache-dir`);
- creating the registry makes no network calls.

**RDAP query (step 2):**
- an expiry that's fine, one in the warning window, one already expired;
- 404;
- no expiration event (falls back to WHOIS);
- dates with fractional seconds, and with no timezone;
- failover across several base URLs;
- IDN conversion;
- the whole-check deadline;
- the bootstrap fetch ignores the monitor's proxy, and the domain query uses it;
- a private-address target blocked through a proxy.

**WHOIS (step 3):** see items 3–6 in §3.

---

## 2. Issues RDAP fixes simply by being the main path (for TLDs that have RDAP)

| Issue | Why it goes away |
|---|---|
| Registries have shut off port-43 WHOIS (the `.app`/`.dev`/`.page` failures) | RDAP runs over HTTPS, and gTLD registries are required to run it. |
| Stale hardcoded server map | Servers come from IANA's bootstrap file. |
| The "not found" text search gives false "not registered" results | An unregistered domain is an HTTP 404. |
| Truncation (8 KB total, 4 KB per line) loses the expiry | The whole JSON response is decoded, up to 1 MB. |
| The first matching pattern wins even if its date doesn't parse | The expiry is a labelled event. |
| Long list of date formats, including the ambiguous `02/01/2006` | RFC 3339 is required by spec. |
| Thin-registry referral is never followed | For gTLDs the registry's RDAP record includes the expiration. |
| A configured HTTP proxy is not used for WHOIS | RDAP is HTTP, so it honours both SOCKS5 and HTTP proxies. |

## 3. Issues fixed as part of the switch (explicit work items)

1. **TLDs missing from the WHOIS map:** if a TLD has no RDAP and is not in the map, ask
   `whois.iana.org` for its `whois:` line. Cache the answer in memory per TLD: 24h expiry, short
   wait after a failure.
2. **Stale map entries:** change `.info` to `whois.nic.info`. Trim the map to overrides only
   (decision 2).
3. **WHOIS date parsing:** go through every match of every pattern and use the first date that
   parses.
4. **Missing WHOIS date formats:** add fractional-second RFC 3339 and tolerate extra whitespace.
5. **WHOIS "not found" check:** try to parse an expiry first. Only if there is none, look for
   not-found phrases, matched as whole lines.
6. **WHOIS read limits:** raise to ~64 KB total and ~16 KB per line.
7. **Threshold test:** `TestDomainExpiryThresholds` should call the real function instead of
   copying its math.
8. **IDN domains:** punycode conversion; this affects both paths.

## 4. What still won't work well for domains that fall through to WHOIS

- **Format variation:** unusual free-text layouts still fail until a pattern is added for them.
- **Slash dates:** `NN/NN/YYYY` is still ambiguous (decision 3).
- **Registries that don't publish expiry:** some ccTLDs withhold it. We can only report it
  clearly.
- **Rate limits and blocks on port 43:** they are often silent and look like an outage.
- **Firewalls:** many networks block outbound port 43, so the fallback can fail where RDAP would
  work.
- **HTTP proxies:** they can't carry port-43 traffic (decision 4).
- **Thin ccTLD registries:** the registrar referral is still not followed (low impact).
- **IANA can list a dead server:** discovery only knows what IANA publishes.
- **The not-found check is still a heuristic,** just a tighter one.
- **The startup delay window:** for the first 10 minutes, a TLD added to IANA since the
  built-in snapshot falls back to WHOIS (rare and short).

## Decisions

| # | Question | Recommendation | Answer |
|---|---|---|---|
| 1 | RDAP server errors: no WHOIS fallback, or always also try WHOIS? | No fallback | open |
| 2 | Static WHOIS map: trim to overrides, or keep it and let IANA discovery fill gaps? | Trim | open |
| 3 | Slash dates in WHOIS: keep day/month, or drop the format? | Keep day/month | open |
| 4 | HTTP proxy plus WHOIS fallback: fail, or go direct and document it? | Fail | open |
| 5 | Startup delay with only the built-in copy | 10 minutes | open (step 1 uses 10 minutes) |
| 6 | Agent cache: add an optional `-cache-dir` flag, or memory only? | Add the flag | open (step 1 adds the flag) |
