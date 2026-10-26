package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/liguangsheng/wildtoken/internal/apperr"
	"github.com/liguangsheng/wildtoken/internal/appstate"
	"github.com/liguangsheng/wildtoken/internal/authstate"
	"github.com/liguangsheng/wildtoken/internal/db"
	"github.com/liguangsheng/wildtoken/internal/metrics"
	"github.com/liguangsheng/wildtoken/internal/middleware"
	"github.com/liguangsheng/wildtoken/internal/models"
	"github.com/liguangsheng/wildtoken/internal/themes"
)

// Version is the service version reported by the system endpoint.
//
// The release workflow parses this line to check that a tag matches, so its
// shape must stay `const Version = "..."`.
const Version = "0.2.3"

// maxLogListOffset caps how deep the offset-paged log list may reach.
const maxLogListOffset int32 = 100_000

// maxLogSearchChars caps a log search term.
const maxLogSearchChars = 200

// decodeJSON reads a JSON request body, ignoring fields the target does not
// declare.
//
// This is the lenient default because several console forms post a superset of
// what an endpoint reads. The channel form is the clearest case: it serves both
// create and update, so it always sends `clear_api_key`, which only the update
// payload declares.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	return decodeBody(w, r, target, false)
}

// decodeStrictJSON additionally rejects unknown fields, so a typo is reported
// rather than silently ignored. It is reserved for payloads whose clients send
// exactly the declared shape.
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any) error {
	return decodeBody(w, r, target, true)
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any, rejectUnknown bool) error {
	// The writer is what lets MaxBytesReader mark the connection for closing
	// when a body runs over. Passing nil left an oversized request reading on
	// against a connection the server would go on to reuse.
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024))
	if rejectUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return apperr.BadRequest("invalid request body: " + err.Error())
	}
	return nil
}

// pathID reads the {id} path parameter.
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		return 0, apperr.BadRequest("id must be an integer")
	}
	return id, nil
}

// isUniqueViolation reports a UNIQUE constraint failure, which the console
// should see as a bad request rather than an internal error.
func isUniqueViolation(err error) bool { return db.IsUniqueViolation(err) }

// HealthCheck reports that the service and its database are reachable.
func HealthCheck(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := state.DB.ExecContext(r.Context(), "SELECT 1"); err != nil {
			apperr.WriteError(w, apperr.Database(err))
			return
		}
		apperr.WriteJSON(w, http.StatusOK, map[string]string{
			"status": "ok", "service": "WildToken",
		})
	}
}

// ListPublicThemePacks serves the theme packs the console may offer.
func ListPublicThemePacks(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		packs, err := themes.ListPacks(state.Settings.Themes.Dir)
		if err != nil {
			apperr.WriteError(w, apperr.Internal("failed to read theme packs: "+err.Error()))
			return
		}
		apperr.WriteJSON(w, http.StatusOK, packs)
	}
}

// AdminGetRuntimeSettings returns the current operator-editable policy.
func AdminGetRuntimeSettings(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := state.Runtime.Get()
		apperr.WriteJSON(w, http.StatusOK, models.NewRuntimeSettingsOut(&settings))
	}
}

// AdminUpdateRuntimeSettings applies a policy edit under a revision check.
func AdminUpdateRuntimeSettings(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input models.RuntimeSettingsIn
		if err := decodeStrictJSON(w, r, &input); err != nil {
			apperr.WriteError(w, err)
			return
		}
		if err := input.Validate(); err != nil {
			apperr.WriteError(w, apperr.BadRequest(err.Error()))
			return
		}

		updated, err := db.UpdateRuntimeSettings(r.Context(), state.DB, &input)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}

		// A concurrent update that committed a newer revision must not be rolled
		// back by this one's snapshot.
		if current := state.Runtime.Get(); updated.Revision > current.Revision {
			state.Runtime.Set(updated)
		}
		apperr.WriteJSON(w, http.StatusOK, models.NewRuntimeSettingsOut(&updated))
	}
}

func AdminListModelTestPromptTemplates(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		templates, err := db.ListModelTestPromptTemplates(r.Context(), state.DB)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusOK, templates)
	}
}

func AdminCreateModelTestPromptTemplate(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input models.ModelTestPromptTemplateIn
		if err := decodeStrictJSON(w, r, &input); err != nil {
			apperr.WriteError(w, err)
			return
		}
		if err := input.Validate(); err != nil {
			apperr.WriteError(w, apperr.BadRequest(err.Error()))
			return
		}

		template, err := db.CreateModelTestPromptTemplate(r.Context(), state.DB, &input)
		if err != nil {
			if isUniqueViolation(err) {
				apperr.WriteError(w, apperr.BadRequest("prompt template name already exists"))
				return
			}
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusCreated, template)
	}
}

func AdminUpdateModelTestPromptTemplate(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		var input models.ModelTestPromptTemplateIn
		if err := decodeStrictJSON(w, r, &input); err != nil {
			apperr.WriteError(w, err)
			return
		}
		if err := input.Validate(); err != nil {
			apperr.WriteError(w, apperr.BadRequest(err.Error()))
			return
		}

		template, found, err := db.UpdateModelTestPromptTemplate(r.Context(), state.DB, id, &input)
		if err != nil {
			// A rename onto another template's name, answered as creation does.
			if isUniqueViolation(err) {
				apperr.WriteError(w, apperr.BadRequest("prompt template name already exists"))
				return
			}
			apperr.WriteError(w, err)
			return
		}
		if !found {
			apperr.WriteError(w, apperr.NotFound("model test prompt template not found"))
			return
		}
		apperr.WriteJSON(w, http.StatusOK, template)
	}
}

func AdminDeleteModelTestPromptTemplate(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		deleted, err := db.DeleteModelTestPromptTemplate(r.Context(), state.DB, id)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		if !deleted {
			apperr.WriteError(w, apperr.NotFound("model test prompt template not found"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// AdminRotateAdminToken replaces the admin credential under a version check.
func AdminRotateAdminToken(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := middleware.AdminAuthFrom(r.Context())
		if !ok {
			apperr.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		var input models.AdminTokenRotateIn
		if err := decodeStrictJSON(w, r, &input); err != nil {
			apperr.WriteError(w, err)
			return
		}
		if !input.Confirm {
			apperr.WriteError(w, apperr.BadRequest("explicit confirmation is required"))
			return
		}

		token, err := input.ValidatedToken()
		if err != nil {
			apperr.WriteError(w, apperr.BadRequest(err.Error()))
			return
		}
		hash, err := authstate.HashAdminToken(token)
		if err != nil {
			apperr.WriteError(w, apperr.Internal("could not hash admin credential"))
			return
		}

		credential, rotated, err := db.RotateAdminCredential(r.Context(), state.DB, hash,
			auth.CredentialVersion)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		if !rotated {
			apperr.WriteError(w, apperr.Conflict("admin credential version conflict"))
			return
		}

		// The snapshot is published only after the credential transaction
		// commits. Publication is monotonic even if concurrent rotations finish
		// out of order.
		state.Credentials.Publish(credential)
		w.WriteHeader(http.StatusNoContent)
	}
}

// AdminSystemInfo reports service, database, and runtime status.
func AdminSystemInfo(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := state.DB.ExecContext(r.Context(), "SELECT 1")
		databaseOK := err == nil

		var databaseAllocatedBytes *int64
		if databaseOK {
			var allocated int64
			err := state.DB.QueryRowContext(r.Context(),
				"SELECT (SELECT page_count FROM pragma_page_count()) * (SELECT page_size FROM pragma_page_size())").
				Scan(&allocated)
			if err == nil {
				databaseAllocatedBytes = &allocated
			}
		}

		logStats := state.LogStats.Snapshot()
		recentOneMinuteLogCount, err := db.RecentOneMinuteLogCount(r.Context(), state.DB)
		if err != nil {
			recentOneMinuteLogCount = 0
		}

		var enabledUpstreamCount, totalUpstreamCount int64
		if err := state.DB.QueryRowContext(r.Context(),
			"SELECT COALESCE(SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END), 0), COUNT(*) FROM upstreams").
			Scan(&enabledUpstreamCount, &totalUpstreamCount); err != nil {
			// The page still renders, but the zeros it would otherwise show are
			// indistinguishable from having no channels at all.
			slog.Warn("could not count channels for the system info panel", "error", err)
		}

		settings := state.Runtime.Get()
		apperr.WriteJSON(w, http.StatusOK, models.SystemInfoOut{
			Service: "WildToken",
			Version: Version,
			// The startup value, not the effective one: the console shows this as
			// what "inherit" means next to the editable runtime override.
			DefaultUpstreamTimeoutSeconds: state.Settings.Upstream.DefaultTimeoutSeconds,
			UptimeSeconds:                 uint64(time.Since(state.StartedAt).Seconds()),
			CurrentServerTime:             time.Now().Format(time.RFC3339),
			DatabaseOK:                    databaseOK,
			DatabaseAllocatedBytes:        databaseAllocatedBytes,
			TotalLogCount:                 logStats.TotalLogCount,
			LogCount24h:                   logStats.LogCount24h,
			EnabledUpstreamCount:          enabledUpstreamCount,
			TotalUpstreamCount:            totalUpstreamCount,
			RecentOneMinuteLogCount:       recentOneMinuteLogCount,
			RuntimeLogSettings: models.RuntimeLogSettingsSummary{
				LogBodyKeepCount: settings.LogBodyKeepCount,
				LogRetentionDays: settings.LogRetentionDays,
				LogBodyMaxBytes:  settings.LogBodyMaxBytes,
				Revision:         settings.Revision,
			},
			RuntimeMetrics: runtimeMetricsOut(state.Metrics.Snapshot()),
		})
	}
}

// AdminRuntimeMetrics reports the in-process counters alone.
func AdminRuntimeMetrics(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apperr.WriteJSON(w, http.StatusOK, runtimeMetricsOut(state.Metrics.Snapshot()))
	}
}

func runtimeMetricsOut(snapshot metrics.Snapshot) models.RuntimeMetricsOut {
	return models.RuntimeMetricsOut{
		ActiveSSEStreams:          snapshot.ActiveSSEStreams,
		SSECompletedTotal:         snapshot.SSECompletedTotal,
		SSEClientDisconnectsTotal: snapshot.SSEClientDisconnectsTotal,
		SSERecentDisconnects10m:   snapshot.SSERecentDisconnects10m,
		SSEUpstreamErrorsTotal:    snapshot.SSEUpstreamErrorsTotal,
		LogQueueDepth:             snapshot.LogQueueDepth,
		LogWrittenTotal:           snapshot.LogWrittenTotal,
		LogWriteBatchesTotal:      snapshot.LogWriteBatchesTotal,
		LogDroppedTotal:           snapshot.LogDroppedTotal,
		LogWriteFailuresTotal:     snapshot.LogWriteFailuresTotal,
		SlowDBOperationsTotal:     snapshot.SlowDBOperationsTotal,
		Cleanup: models.RuntimeCleanupMetricsOut{
			Active:                  snapshot.CleanupActive,
			RunsTotal:               snapshot.CleanupRunsTotal,
			ErrorsTotal:             snapshot.CleanupErrorsTotal,
			RowsClearedTotal:        snapshot.CleanupRowsClearedTotal,
			BatchesTotal:            snapshot.CleanupBatchesTotal,
			CurrentRowsCleared:      snapshot.CleanupCurrentRowsCleared,
			CurrentBatches:          snapshot.CleanupCurrentBatches,
			LastStartedUnixSeconds:  snapshot.CleanupLastStartedUnixSeconds,
			LastFinishedUnixSeconds: snapshot.CleanupLastFinishedUnixSeconds,
			LastDurationMs:          snapshot.CleanupLastDurationMs,
			LastRowsCleared:         snapshot.CleanupLastRowsCleared,
		},
	}
}

// AdminListTokens returns every downstream API token.
func AdminListTokens(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokens, err := db.ListTokens(r.Context(), state.DB)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusOK, tokens)
	}
}

func AdminGetToken(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		token, found, err := db.GetToken(r.Context(), state.DB, id)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		if !found {
			apperr.WriteError(w, apperr.NotFound("token not found"))
			return
		}
		apperr.WriteJSON(w, http.StatusOK, token)
	}
}

func AdminCreateToken(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input := models.APITokenIn{Enabled: true}
		if err := decodeStrictJSON(w, r, &input); err != nil {
			apperr.WriteError(w, err)
			return
		}
		if err := input.Validate(); err != nil {
			apperr.WriteError(w, apperr.BadRequest(err.Error()))
			return
		}

		created, err := db.CreateToken(r.Context(), state.DB, &input)
		if err != nil {
			if isUniqueViolation(err) {
				apperr.WriteError(w, apperr.BadRequest("token name or value already exists"))
				return
			}
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusCreated, created)
	}
}

func AdminUpdateToken(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		// Read once, decoded twice: the keys it names, then their values.
		var body json.RawMessage
		if err := decodeJSON(w, r, &body); err != nil {
			apperr.WriteError(w, err)
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			apperr.WriteError(w, apperr.BadRequest("invalid request body: "+err.Error()))
			return
		}
		// A full replacement: a field left out is refused rather than read as
		// its most permissive value.
		if missing := models.MissingTokenFields(fields); len(missing) > 0 {
			apperr.WriteError(w, apperr.BadRequest("missing fields: "+strings.Join(missing, ", ")))
			return
		}
		var input models.APITokenUpdateIn
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			apperr.WriteError(w, apperr.BadRequest("invalid request body: "+err.Error()))
			return
		}
		if err := input.Validate(); err != nil {
			apperr.WriteError(w, apperr.BadRequest(err.Error()))
			return
		}

		// UpdateToken reads the existing row to decide whether the expiry
		// changed, and reports NotFound itself, so no separate check is needed.
		updated, err := db.UpdateToken(r.Context(), state.DB, id, &input)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusOK, updated)
	}
}

func AdminSetTokenEnabled(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		var input models.TokenEnabledIn
		if err := decodeStrictJSON(w, r, &input); err != nil {
			apperr.WriteError(w, err)
			return
		}
		enabled, err := input.Value()
		if err != nil {
			apperr.WriteError(w, apperr.BadRequest(err.Error()))
			return
		}

		if _, found, err := db.GetToken(r.Context(), state.DB, id); err != nil {
			apperr.WriteError(w, err)
			return
		} else if !found {
			apperr.WriteError(w, apperr.NotFound("token not found"))
			return
		}

		updated, err := db.SetTokenEnabled(r.Context(), state.DB, id, enabled)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusOK, updated)
	}
}

// AdminResetTokenUsage clears a token's consumed total.
func AdminResetTokenUsage(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		updated, err := db.ResetTokenUsage(r.Context(), state.DB, id)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusOK, updated)
	}
}

func AdminDeleteToken(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		deleted, err := db.DeleteToken(r.Context(), state.DB, id)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		if !deleted {
			apperr.WriteError(w, apperr.NotFound("token not found"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// AdminListLogs serves one page of request logs.
func AdminListLogs(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		limit := clampInt32(queryInt32(query.Get("limit"), 50), 1, 200)
		// SQLite reaches an OFFSET by scanning and discarding the rows before
		// it, so an unbounded one lets a single request walk the whole log
		// table and hold one of the few pooled connections for as long as that
		// takes. Deep pages are what the cursor endpoint is for.
		offset := clampInt32(queryInt32(query.Get("offset"), 0), 0, maxLogListOffset)

		// The search term becomes eight LIKE patterns evaluated against every
		// row the query examines, so its length is a multiplier on the cost of
		// the whole scan. Nothing an operator types to find a request is longer
		// than this.
		filter := db.LogFilter{
			UpstreamID: optionalQueryInt64(query.Get("upstream_id")),
			Search:     boundedQueryString(query.Get("search"), maxLogSearchChars),
			ClientType: boundedQueryString(query.Get("client_type"), maxLogSearchChars),
		}
		if status := query.Get("status"); status == "2xx" || status == "4xx" ||
			status == "5xx" || status == "none" {
			filter.Status = &status
		}

		var cursor *db.LogCursor
		beforeCreatedAt := strings.TrimSpace(query.Get("before_created_at"))
		beforeID := optionalQueryInt64(query.Get("before_id"))
		if beforeCreatedAt != "" && beforeID != nil {
			cursor = &db.LogCursor{CreatedAt: beforeCreatedAt, ID: *beforeID}
		}

		// One extra row is requested, so the page knows whether more follow
		// without a second count query.
		items, err := db.ListLogs(r.Context(), state.DB, limit+1, offset, cursor, filter)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}

		hasMore := int32(len(items)) > limit
		if hasMore {
			items = items[:limit]
		}

		var nextCursor *models.RequestLogCursorOut
		if hasMore && len(items) > 0 {
			last := items[len(items)-1]
			nextCursor = &models.RequestLogCursorOut{CreatedAt: last.CreatedAt, ID: last.ID}
		}

		recentRate, err := db.RecentOneMinuteLogRate(r.Context(), state.DB)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}

		// In-flight requests belong to the newest page only, and are left for the
		// console to filter: they carry the same fields the filters read, and the
		// stream already hands them over unfiltered.
		var active []models.ActiveRequestOut
		var activeTotal int
		if cursor == nil && offset == 0 {
			snapshot := state.ActiveRequests.Snapshot()
			active, activeTotal = snapshot.Requests, snapshot.Total
		}

		apperr.WriteJSON(w, http.StatusOK, models.RequestLogPage{
			Items:       items,
			HasMore:     hasMore,
			Active:      active,
			ActiveTotal: activeTotal,
			RecentRPM:   recentRate.RequestCount,
			RecentTPM:   recentRate.TotalTokens,
			NextCursor:  nextCursor,
		})
	}
}

// activePollInterval is how often the stream looks for a change to the
// in-flight set.
//
// Nothing is sent when the version has not moved, so an idle gateway costs one
// comparison a second. The requests are not re-sent to advance their elapsed
// time either: the console counts that up from what it was given.
const activePollInterval = time.Second

// writeActiveRequests emits the in-flight set and reports the version it sent.
func writeActiveRequests(w http.ResponseWriter, flusher http.Flusher,
	state *appstate.State) uint64 {
	snapshot := state.ActiveRequests.Snapshot()
	requests := snapshot.Requests
	if requests == nil {
		requests = []models.ActiveRequestOut{}
	}
	// total travels with the list because the list is capped: concurrency is
	// read from the count, not from how many rows fit.
	encoded, err := json.Marshal(map[string]any{
		"requests": requests,
		"total":    snapshot.Total,
	})
	if err != nil {
		// The set is built from plain values, so this cannot fail without a
		// programming error. An empty set is the safe reading of one.
		encoded = []byte(`{"requests":[],"total":0}`)
	}
	fmt.Fprintf(w, "event: active\ndata: %s\n\n", encoded)
	flusher.Flush()
	return snapshot.Version
}

// AdminStreamLogs streams lightweight list-row events for request logs that
// have committed to SQLite, plus the set of requests still in flight.
//
// The endpoint intentionally does not replay historical rows. A disconnected or
// lagged client reloads the normal paginated endpoint, which remains the source
// of truth and keeps cursor pagination stable. A lagged one learns it lagged
// from a resync event, sent when rows it had no room for were dropped.
//
// In-flight requests are sent as a whole set rather than as per-request events.
// The set is small and short-lived, and one snapshot cannot leave a console
// holding a row for a request that ended while it was not listening.
func AdminStreamLogs(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := middleware.AdminAuthFrom(r.Context())
		if !ok {
			apperr.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		flusher, canFlush := w.(http.Flusher)
		if !canFlush {
			apperr.WriteError(w, apperr.Internal("streaming is not supported"))
			return
		}

		subscription := state.LogWriter.Subscribe()
		defer subscription.Close()
		events := subscription.Events()

		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-store")
		w.Header().Set("connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// The console starts with an empty list, so the opening snapshot is sent
		// unconditionally; after that only a changed version is worth bytes.
		sentActiveVersion := writeActiveRequests(w, flusher, state)
		activePoll := time.NewTicker(activePollInterval)
		defer activePoll.Stop()

		// Without it a console that fell behind kept a list with holes and no
		// sign of them.
		resyncIfMissed := func() {
			if subscription.TakeMissed() {
				fmt.Fprint(w, "event: resync\ndata: {}\n\n")
				flusher.Flush()
			}
		}

		// A rotation invalidates this stream, so a revoked operator stops
		// receiving live logs without waiting for their connection to drop.
		authCheck := time.NewTicker(15 * time.Second)
		defer authCheck.Stop()

		// Rates travel with each log, so once traffic stopped the console kept
		// showing the last log's figures long after the window had emptied.
		rateRefresh := time.NewTicker(rateRefreshInterval)
		defer rateRefresh.Stop()
		var sentRate db.RecentLogRate

		for {
			select {
			case <-r.Context().Done():
				return

			case <-state.Stopping:
				return

			case event, open := <-events:
				if !open {
					return
				}
				if state.Credentials.Version() != auth.CredentialVersion {
					return
				}
				if event.RecentRPM != nil && event.RecentTPM != nil {
					sentRate = db.RecentLogRate{RequestCount: *event.RecentRPM, TotalTokens: *event.RecentTPM}
				}
				encoded, err := json.Marshal(event)
				if err != nil {
					encoded = fmt.Appendf(nil, `{"log":{"id":%d}}`, event.Log.ID)
				}
				fmt.Fprintf(w, "event: log\nid: %d\ndata: %s\n\n", event.Log.ID, encoded)
				flusher.Flush()
				resyncIfMissed()

				// A committed row is the one moment the two views are certain to
				// disagree: the request left the in-flight set before its log was
				// written, so without this the console shows it twice until the
				// next poll.
				if state.ActiveRequests.Version() != sentActiveVersion {
					sentActiveVersion = writeActiveRequests(w, flusher, state)
				}

			case <-activePoll.C:
				if state.Credentials.Version() != auth.CredentialVersion {
					return
				}
				if state.ActiveRequests.Version() != sentActiveVersion {
					sentActiveVersion = writeActiveRequests(w, flusher, state)
				}
				resyncIfMissed()

			case <-rateRefresh.C:
				ctx, cancel := context.WithTimeout(r.Context(), rateRefreshInterval)
				rate, err := db.RecentOneMinuteLogRate(ctx, state.DB)
				cancel()
				if err == nil && rate != sentRate {
					sentRate = rate
					fmt.Fprintf(w, "event: rate\ndata: {\"recent_rpm\":%d,\"recent_tpm\":%d}\n\n",
						rate.RequestCount, rate.TotalTokens)
					flusher.Flush()
				}

			case <-authCheck.C:
				if state.Credentials.Version() != auth.CredentialVersion {
					return
				}
				// A comment frame keeps intermediaries from closing an idle
				// connection.
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}

// analyticsContext bounds a dashboard query as LogQueryTimeout bounds a log
// listing. These scan whole windows of request_logs on the few pooled
// connections the proxy's token lookups and the log writer wait for; left
// unbounded on a large table, a dashboard load held them for as long as it
// took.
func analyticsContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), db.LogQueryTimeout)
}

// rateRefreshInterval is how often the live stream re-reads the one-minute
// rates, which change as the window slides even when no log arrives. A var so
// a test need not wait it out.
var rateRefreshInterval = 10 * time.Second

const dashboardDateLayout = "2006-01-02"

// dashboardRangeLayouts accepts a bare date or a wall-clock instant. The date
// form is kept because that is what the stored console preference holds.
var dashboardRangeLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	dashboardDateLayout,
}

// parseDashboardBound reports whether the value named a whole day, which
// decides if the upper bound is rounded up to the following midnight.
func parseDashboardBound(value string, zone *time.Location) (time.Time, bool, error) {
	for _, layout := range dashboardRangeLayouts {
		parsed, err := time.ParseInLocation(layout, value, zone)
		if err == nil {
			return parsed, layout == dashboardDateLayout, nil
		}
	}
	return time.Time{}, false, errors.New("unrecognised timestamp")
}

// dashboardZone reads the console's UTC offset, tz_offset in minutes east of
// UTC.
//
// The dashboard's days are the operator's: "today", custom dates and bucket
// edges all follow their browser. Read in the server's zone instead, a custom
// range shifted by the difference between the two, and so did "today". A
// caller that sends no offset gets the server's zone, as before.
func dashboardZone(value string) (*time.Location, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Local, nil
	}
	minutes, err := strconv.Atoi(value)
	if err != nil || minutes < -14*60 || minutes > 14*60 {
		return nil, apperr.BadRequest("tz_offset must be minutes east of UTC, between -840 and 840")
	}
	return time.FixedZone("", minutes*60), nil
}

type dashboardRangeSelection struct {
	Value   string
	Label   string
	Window  db.LogTopWindow
	StartAt string
	EndAt   string
	// OffsetSeconds is the zone's current UTC offset, for bucket alignment.
	OffsetSeconds int64
}

func parseDashboardRange(value, startValue, endValue, fallback string,
	zone *time.Location) (dashboardRangeSelection, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	window, ok := db.ParseLogTopWindow(value)
	if !ok {
		return dashboardRangeSelection{}, apperr.BadRequest(
			"range must be one of: today, 1d, 3d, 7d, 30d, all, default, custom")
	}

	now := time.Now().In(zone)
	_, offset := now.Zone()
	selection := dashboardRangeSelection{Value: value, Window: window, OffsetSeconds: int64(offset)}
	switch value {
	case "today":
		selection.Label = "今天"
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, zone)
		selection.StartAt = midnight.UTC().Format(models.TimestampFormat)
	case "1d":
		selection.Label = "最近 24 小时"
	case "3d":
		selection.Label = "最近 3 天"
	case "7d":
		selection.Label = "最近 7 天"
	case "30d":
		selection.Label = "最近 30 天"
	case "all":
		selection.Label = "全部时间"
	case "default":
		selection.Label = "所有窗口"
	case "custom":
		startValue = strings.TrimSpace(startValue)
		endValue = strings.TrimSpace(endValue)
		if startValue == "" || endValue == "" {
			return dashboardRangeSelection{}, apperr.BadRequest(
				"start_date and end_date are required when range is custom")
		}
		// Only the upper bound cares whether it named a whole day; a start is
		// taken at its own midnight either way.
		startDate, _, startErr := parseDashboardBound(startValue, zone)
		endDate, endIsWholeDay, endErr := parseDashboardBound(endValue, zone)
		if startErr != nil || endErr != nil {
			return dashboardRangeSelection{}, apperr.BadRequest(
				"start_date and end_date must use YYYY-MM-DD or YYYY-MM-DDTHH:MM[:SS]")
		}
		// A bare end date means the whole of that day, so the exclusive upper
		// bound is the following midnight. A precise instant is used as given.
		endBound := endDate
		if endIsWholeDay {
			endBound = endDate.AddDate(0, 0, 1)
		}
		if !startDate.Before(endBound) {
			return dashboardRangeSelection{}, apperr.BadRequest(
				"start_date must be before end_date")
		}
		if endBound.After(startDate.AddDate(0, 0, 366)) {
			return dashboardRangeSelection{}, apperr.BadRequest(
				"custom date range must not exceed 366 days")
		}
		selection.Label = startValue + " 至 " + endValue
		selection.StartAt = startDate.UTC().Format(models.TimestampFormat)
		selection.EndAt = endBound.UTC().Format(models.TimestampFormat)
	}
	return selection, nil
}

// AdminTokenUsageStats reports token usage for the selected dashboard range.
func AdminTokenUsageStats(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		zone, err := dashboardZone(query.Get("tz_offset"))
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		selection, err := parseDashboardRange(
			query.Get("range"), query.Get("start_date"), query.Get("end_date"), "default", zone)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}

		stats := state.LogStats.Snapshot().TokenUsage
		if selection.Value == "default" {
			apperr.WriteJSON(w, http.StatusOK, stats)
			return
		}

		// The cache's "today" starts at the server's midnight; an operator in
		// another zone gets theirs from the database.
		_, serverOffset := time.Now().Zone()
		var usage models.TokenUsageWindowOut
		switch selection.Window {
		case db.LogTopWindowToday:
			usage = stats.Today
			if selection.OffsetSeconds != int64(serverOffset) {
				ctx, cancel := analyticsContext(r)
				defer cancel()
				usage, err = db.QueryTokenUsage(ctx, state.DB, selection.Window,
					selection.StartAt, selection.EndAt)
				if err != nil {
					apperr.WriteError(w, err)
					return
				}
			}
		case db.LogTopWindowOneDay:
			usage = stats.OneDay
		case db.LogTopWindowSevenDays:
			usage = stats.SevenDays
		case db.LogTopWindowThirtyDays:
			usage = stats.ThirtyDays
		case db.LogTopWindowAll:
			usage = stats.AllTime
		default:
			ctx, cancel := analyticsContext(r)
			defer cancel()
			usage, err = db.QueryTokenUsage(ctx, state.DB, selection.Window,
				selection.StartAt, selection.EndAt)
			if err != nil {
				apperr.WriteError(w, err)
				return
			}
		}

		stats.Today = usage
		stats.Range = selection.Value
		stats.RangeLabel = selection.Label
		apperr.WriteJSON(w, http.StatusOK, stats)
	}
}

// AdminLogOverview returns range-scoped dashboard aggregates: request and
// error totals, status buckets, latency stats, and a time-bucketed latency
// series. The dashboard's KPI cards and charts read this instead of doing
// client-side math over whatever logs happen to be loaded.
func AdminLogOverview(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		zone, err := dashboardZone(query.Get("tz_offset"))
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		selection, err := parseDashboardRange(
			query.Get("range"), query.Get("start_date"), query.Get("end_date"), "today", zone)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}

		ctx, cancel := analyticsContext(r)
		defer cancel()
		overview, err := db.LogOverview(ctx, state.DB,
			selection.Window, selection.StartAt, selection.EndAt, selection.OffsetSeconds)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		overview.Range = selection.Value
		overview.RangeLabel = selection.Label
		apperr.WriteJSON(w, http.StatusOK, overview)
	}
}

// AdminTopLogStats ranks models and channels over a window.
func AdminTopLogStats(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		zone, err := dashboardZone(query.Get("tz_offset"))
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		selection, err := parseDashboardRange(
			query.Get("window"), query.Get("start_date"), query.Get("end_date"), "today", zone)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		limit := clampInt64(queryInt64(query.Get("limit"), 10), 1, 20)

		ctx, cancel := analyticsContext(r)
		defer cancel()
		stats, err := db.TopLogStatsRange(ctx, state.DB, selection.Window,
			selection.StartAt, selection.EndAt, limit)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusOK, stats)
	}
}

// AdminGetLogDetail returns one log with its captured payloads.
func AdminGetLogDetail(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		detail, found, err := db.GetLogDetail(r.Context(), state.DB, id)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		if !found {
			apperr.WriteError(w, apperr.NotFound("request log not found"))
			return
		}
		apperr.WriteJSON(w, http.StatusOK, detail)
	}
}

// AdminGetLogSnapshot returns one captured payload of a log, or null when
// that payload was never stored.
func AdminGetLogSnapshot(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		field, ok := db.ParseLogSnapshotField(chi.URLParam(r, "field"))
		if !ok {
			apperr.WriteError(w, apperr.BadRequest("unknown snapshot field"))
			return
		}
		snapshot, found, err := db.GetLogSnapshot(r.Context(), state.DB, id, field)
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		if !found {
			apperr.WriteError(w, apperr.NotFound("request log not found"))
			return
		}
		if snapshot == nil {
			snapshot = json.RawMessage("null")
		}
		apperr.WriteJSON(w, http.StatusOK, snapshot)
	}
}

// AdminUpstreamHealthHistory reports per-channel hourly success rate and
// latency over the trailing hours (default 24), for the channel cards' mini
// health trend.
func AdminUpstreamHealthHistory(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hours := clampInt64(queryInt64(r.URL.Query().Get("hours"), 24), 1, 24*7)
		zone, err := dashboardZone(r.URL.Query().Get("tz_offset"))
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		_, offset := time.Now().In(zone).Zone()
		ctx, cancel := analyticsContext(r)
		defer cancel()
		health, err := db.UpstreamHealthHistory(ctx, state.DB, hours, int64(offset))
		if err != nil {
			apperr.WriteError(w, err)
			return
		}
		apperr.WriteJSON(w, http.StatusOK, map[string]any{
			"hours":   hours,
			"entries": health,
		})
	}
}

func queryInt32(value string, fallback int32) int32 {
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return fallback
	}
	return int32(parsed)
}

func queryInt64(value string, fallback int64) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func optionalQueryInt64(value string) *int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

// boundedQueryString trims a query value and caps its length, for values that
// end up inside a query's cost rather than beside it.
func boundedQueryString(value string, maxChars int) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	if utf8.RuneCountInString(trimmed) > maxChars {
		trimmed = truncateRunes(trimmed, maxChars)
	}
	return &trimmed
}

func clampInt32(value, low, high int32) int32 { return min(max(value, low), high) }

func clampInt64(value, low, high int64) int64 { return min(max(value, low), high) }

// upstreamSparklinePoint is one 30-minute request-count bucket.
type upstreamSparklinePoint struct {
	Bucket string `json:"bucket"`
	Count  int64  `json:"count"`
}

// upstreamChannelStats is the per-channel block of the bulk stats response.
type upstreamChannelStats struct {
	Sparkline      []upstreamSparklinePoint `json:"sparkline"`
	TotalRequests  int64                    `json:"totalRequests"`
	CacheHitRate   float64                  `json:"cacheHitRate"`
	AvgTokensPer1M float64                  `json:"avgTokensPer1M"`
}

// AdminGetUpstreamsStats returns card statistics for every channel in one
// response, keyed by upstream id. One request replaces the per-channel
// endpoint the card view used before: with N channels that meant N HTTP
// round trips and 4N queries firing together every time the console's stats
// cache expired.
func AdminGetUpstreamsStats(state *appstate.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats := map[string]*upstreamChannelStats{}
		channel := func(id int64) *upstreamChannelStats {
			key := strconv.FormatInt(id, 10)
			entry, ok := stats[key]
			if !ok {
				entry = &upstreamChannelStats{Sparkline: []upstreamSparklinePoint{}}
				stats[key] = entry
			}
			return entry
		}

		// 6-hour sparkline: 30-minute buckets, grouped per channel.
		sixHoursAgo := time.Now().Add(-6 * time.Hour)
		ctx, cancel := analyticsContext(r)
		defer cancel()
		rows, err := state.DB.QueryContext(ctx, `
			SELECT
				upstream_id,
				strftime('%Y-%m-%d %H:%M:00', created_at,
					'-' || (strftime('%M', created_at) % 30) || ' minutes') AS bucket,
				COUNT(*) AS count
			FROM request_logs
			WHERE upstream_id IS NOT NULL AND created_at >= datetime(?, 'unixepoch')
			GROUP BY upstream_id, bucket
			ORDER BY upstream_id, bucket ASC
		`, sixHoursAgo.Unix())
		if err != nil {
			apperr.WriteError(w, apperr.Internal("failed to fetch sparkline data"))
			return
		}
		defer rows.Close()
		for rows.Next() {
			var upstreamID int64
			var point upstreamSparklinePoint
			if err := rows.Scan(&upstreamID, &point.Bucket, &point.Count); err != nil {
				apperr.WriteError(w, apperr.Internal("failed to scan sparkline row"))
				return
			}
			entry := channel(upstreamID)
			entry.Sparkline = append(entry.Sparkline, point)
		}
		if err := rows.Err(); err != nil {
			apperr.WriteError(w, apperr.Internal("failed to read sparkline data"))
			return
		}

		// Lifetime totals per channel: request count, cache hit rate over
		// prompt tokens, and average tokens per thousand requests (the
		// cost proxy — no pricing data is stored anywhere in this project).
		totals, err := state.DB.QueryContext(r.Context(), `
			SELECT
				upstream_id,
				COUNT(*),
				COALESCE(SUM(prompt_cached_tokens), 0),
				COALESCE(SUM(prompt_tokens), 0),
				COALESCE(SUM(total_tokens), 0)
			FROM request_logs
			WHERE upstream_id IS NOT NULL
			GROUP BY upstream_id
		`)
		if err != nil {
			apperr.WriteError(w, apperr.Internal("failed to fetch channel totals"))
			return
		}
		defer totals.Close()
		for totals.Next() {
			var upstreamID, requests, cachedTokens, promptTokens, totalTokens int64
			if err := totals.Scan(&upstreamID, &requests, &cachedTokens, &promptTokens, &totalTokens); err != nil {
				apperr.WriteError(w, apperr.Internal("failed to scan channel totals"))
				return
			}
			entry := channel(upstreamID)
			entry.TotalRequests = requests
			if promptTokens > 0 {
				entry.CacheHitRate = float64(cachedTokens) / float64(promptTokens) * 100
			}
			if requests > 0 {
				entry.AvgTokensPer1M = float64(totalTokens) / float64(requests) * 1000
			}
		}
		if err := totals.Err(); err != nil {
			apperr.WriteError(w, apperr.Internal("failed to read channel totals"))
			return
		}

		apperr.WriteJSON(w, http.StatusOK, map[string]any{"stats": stats})
	}
}
