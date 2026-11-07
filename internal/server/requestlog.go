package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/y0f/asura/internal/httputil"
	"github.com/y0f/asura/internal/storage"
)

const (
	requestLogChanSize      = 4096
	requestLogBatchSize     = 100
	requestLogFlushInterval = 5 * time.Second
)

// heartbeatPathPrefix is the heartbeat ping route; the segment after it is the
// monitor's secret token.
const heartbeatPathPrefix = "/api/v1/heartbeat/"

// heartbeatDigestLen is how many hex characters of sha256(token) a logged
// heartbeat path keeps: enough to group pings per monitor, too few to be
// used as the token or to identify it outside this database.
const heartbeatDigestLen = 12

var _monitorIDRe = regexp.MustCompile(`(?:^/monitors/|^/api/v1/monitors/|^/api/v1/badge/)(\d+)`)

type RequestLogWriter struct {
	ch     chan *storage.RequestLog
	store  storage.Store
	logger *slog.Logger
}

func NewRequestLogWriter(store storage.Store, logger *slog.Logger) *RequestLogWriter {
	return &RequestLogWriter{
		ch:     make(chan *storage.RequestLog, requestLogChanSize),
		store:  store,
		logger: logger,
	}
}

func (w *RequestLogWriter) Send(log *storage.RequestLog) {
	select {
	case w.ch <- log:
	default:
	}
}

func (w *RequestLogWriter) Run(ctx context.Context) {
	ticker := time.NewTicker(requestLogFlushInterval)
	defer ticker.Stop()

	var batch []*storage.RequestLog

	flush := func(fctx context.Context) {
		if len(batch) == 0 {
			return
		}
		if err := w.store.InsertRequestLogBatch(fctx, batch); err != nil {
			w.logger.Error("request log batch insert failed", "count", len(batch), "error", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Persist everything already queued; Send never blocks, so the
			// channel only holds logs from requests that have completed.
			fctx := context.Background()
			for {
				select {
				case log := <-w.ch:
					batch = append(batch, log)
					if len(batch) >= requestLogBatchSize {
						flush(fctx)
					}
				default:
					flush(fctx)
					return
				}
			}
		case log := <-w.ch:
			batch = append(batch, log)
			if len(batch) >= requestLogBatchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		}
	}
}

func requestLogMiddleware(writer *RequestLogWriter, basePath string, trustedNets []net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path

			if shouldSkipLog(path, basePath) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			sw := &httputil.StatusWriter{ResponseWriter: w, Code: 200}
			next.ServeHTTP(sw, r)
			latency := time.Since(start).Milliseconds()

			trimmed := path
			if basePath != "" {
				trimmed = strings.TrimPrefix(path, basePath)
				if trimmed == "" {
					trimmed = "/"
				}
			}

			ip := httputil.ExtractIP(r, trustedNets)

			// Heartbeat tokens are stored only as sha256 digests, so the
			// plaintext must not reach request_logs either.
			if redacted, ok := redactHeartbeatPath(trimmed); ok {
				trimmed = redacted
				path = basePath + redacted
			}

			log := &storage.RequestLog{
				Method:     r.Method,
				Path:       path,
				StatusCode: sw.Code,
				LatencyMs:  latency,
				ClientIP:   ip,
				UserAgent:  truncate(r.UserAgent(), 512),
				Referer:    truncate(r.Referer(), 512),
				RouteGroup: classifyRoute(trimmed),
				CreatedAt:  start.UTC(),
			}

			if mid := extractMonitorID(trimmed); mid > 0 {
				log.MonitorID = &mid
			}

			writer.Send(log)
		})
	}
}

func shouldSkipLog(path, basePath string) bool {
	trimmed := path
	if basePath != "" {
		trimmed = strings.TrimPrefix(path, basePath)
	}
	if strings.HasPrefix(trimmed, "/static/") {
		return true
	}
	if trimmed == "/api/v1/health" {
		return true
	}
	return false
}

// redactHeartbeatPath replaces the token in a heartbeat ping path (with the
// base path already stripped) by "sha256:" and a prefix of the token's
// at-rest digest, which matches the start of heartbeats.token.
func redactHeartbeatPath(path string) (string, bool) {
	token, ok := strings.CutPrefix(path, heartbeatPathPrefix)
	if !ok || token == "" {
		return path, false
	}
	return heartbeatPathPrefix + "sha256:" + storage.HashToken(token)[:heartbeatDigestLen], true
}

func classifyRoute(path string) string {
	switch {
	case strings.HasPrefix(path, heartbeatPathPrefix):
		return "heartbeat"
	case strings.HasPrefix(path, "/api/v1/badge/"):
		return "badge"
	case strings.HasPrefix(path, "/api/v1/"):
		return "api"
	case path == "/login" || path == "/logout":
		return "auth"
	case strings.HasPrefix(path, "/metrics"):
		return "metrics"
	case path == "/status" || strings.HasPrefix(path, "/status/"):
		return "status"
	default:
		return "web"
	}
}

func extractMonitorID(path string) int64 {
	matches := _monitorIDRe.FindStringSubmatch(path)
	if len(matches) < 2 {
		return 0
	}
	id, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
