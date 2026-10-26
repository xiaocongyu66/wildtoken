package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// ModelTestRequest is one console-issued inference test.
//
// Protocol is the wire format the request is built in. It is a fixed set
// rather than a stored template, because each value maps to a request shape
// and header set the gateway already knows how to produce.
type ModelTestRequest struct {
	Model            string `json:"model"`
	Protocol         string `json:"protocol"`
	PromptTemplateID int64  `json:"prompt_template_id"`
	Prompt           string `json:"prompt"`
}

func (r *ModelTestRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" || len(r.Model) > 500 {
		return ErrString("model must be between 1 and 500 bytes")
	}
	switch r.Protocol {
	case "responses", "chat_completions", "messages":
	default:
		return ErrString("protocol must be responses, chat_completions, or messages")
	}
	// A typed prompt makes the template unnecessary; requiring one anyway left
	// the test unusable once every template had been deleted.
	if strings.TrimSpace(r.Prompt) == "" && r.PromptTemplateID < 1 {
		return ErrString("prompt_template_id must be positive when prompt is empty")
	}
	if len(r.Prompt) > 20000 {
		return ErrString("prompt must be at most 20000 bytes")
	}
	return nil
}

// DebugRequest is one request from the debug page: a hand-edited body sent
// through a channel as-is.
//
// Model feeds the protocol headers and the log row. It is separate from the
// body's model field because the two can differ on purpose: "opus[1m]" asks
// for the 1M beta header while the body carries the plain id.
type DebugRequest struct {
	Protocol string          `json:"protocol"`
	Model    string          `json:"model"`
	Body     json.RawMessage `json:"body"`
}

// debugBodyMaxBytes bounds the edited body. Four times the prompt limit
// leaves room for a system prompt and tool definitions.
const debugBodyMaxBytes = 1 << 20

func (r *DebugRequest) Validate() error {
	switch r.Protocol {
	case "responses", "chat_completions", "messages", "images":
	default:
		return ErrString("protocol must be responses, chat_completions, messages, or images")
	}
	if len(r.Model) > 500 {
		return ErrString("model must be at most 500 bytes")
	}
	if len(r.Body) > debugBodyMaxBytes {
		return ErrString("body must be at most 1 MiB")
	}
	// Every protocol takes a JSON object. An array or a bare string would go
	// upstream and come back as a 400 that looks like the channel's fault.
	if trimmed := bytes.TrimSpace(r.Body); len(trimmed) == 0 || trimmed[0] != '{' {
		return ErrString("body must be a JSON object")
	}
	return nil
}

type ModelTestPromptTemplate struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type ModelTestPromptTemplateIn struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

func (t *ModelTestPromptTemplateIn) Validate() error {
	if strings.TrimSpace(t.Name) == "" || utf8.RuneCountInString(t.Name) > 80 {
		return ErrString("prompt template name must be between 1 and 80 characters")
	}
	if strings.TrimSpace(t.Prompt) == "" || len(t.Prompt) > 20000 {
		return ErrString("prompt template prompt must be between 1 and 20000 bytes")
	}
	return nil
}

// AdminCredential is the Argon2id snapshot plus the generation it belongs to.
type AdminCredential struct {
	CredentialHash    string
	CredentialVersion int64
}

type AdminTokenRotateIn struct {
	Confirm bool   `json:"confirm"`
	Token   string `json:"token"`
}

func (r *AdminTokenRotateIn) ValidatedToken() (string, error) {
	return validateAdminTokenValue(r.Token, AdminTokenRotateMinLen)
}

// AdminTokenMinLen is the minimum admin token length at bootstrap.
//
// Throttling bounds how fast a token can be guessed; length is what makes the
// bound irrelevant. Existing credentials are unaffected — only bootstrap and
// rotation pass through here.
const AdminTokenMinLen = 24

// AdminTokenRotateMinLen is the minimum length for a token chosen from the
// console. Rotation is already behind the current credential, so the operator
// keeps the choice the bootstrap path cannot leave open to an unattended value.
const AdminTokenRotateMinLen = 8

// ValidateAdminTokenValue returns the trimmed token when it is strong enough.
func ValidateAdminTokenValue(value string) (string, error) {
	return validateAdminTokenValue(value, AdminTokenMinLen)
}

func validateAdminTokenValue(value string, minLen int) (string, error) {
	token := strings.TrimSpace(value)
	if len(token) < minLen || len(token) > 256 {
		return "", ErrString(fmt.Sprintf("admin token must be between %d and 256 bytes", minLen))
	}
	if !isASCIIGraphic(token) {
		return "", ErrString("admin token must contain only printable ASCII characters without spaces")
	}
	if strings.EqualFold(token, "change-me") {
		return "", ErrString("admin token must not use the known change-me placeholder")
	}
	return token, nil
}

type RuntimeLogSettingsSummary struct {
	LogBodyKeepCount int64 `json:"log_body_keep_count"`
	LogRetentionDays int64 `json:"log_retention_days"`
	LogBodyMaxBytes  int64 `json:"log_body_max_bytes"`
	Revision         int64 `json:"revision"`
}

type RuntimeCleanupMetricsOut struct {
	Active                  bool    `json:"active"`
	RunsTotal               uint64  `json:"runs_total"`
	ErrorsTotal             uint64  `json:"errors_total"`
	RowsClearedTotal        uint64  `json:"rows_cleared_total"`
	BatchesTotal            uint64  `json:"batches_total"`
	CurrentRowsCleared      uint64  `json:"current_rows_cleared"`
	CurrentBatches          uint64  `json:"current_batches"`
	LastStartedUnixSeconds  *int64  `json:"last_started_unix_seconds,omitempty"`
	LastFinishedUnixSeconds *int64  `json:"last_finished_unix_seconds,omitempty"`
	LastDurationMs          *uint64 `json:"last_duration_ms,omitempty"`
	LastRowsCleared         uint64  `json:"last_rows_cleared"`
}

type RuntimeMetricsOut struct {
	ActiveSSEStreams          uint64                   `json:"active_sse_streams"`
	SSECompletedTotal         uint64                   `json:"sse_completed_total"`
	SSEClientDisconnectsTotal uint64                   `json:"sse_client_disconnects_total"`
	SSERecentDisconnects10m   uint64                   `json:"sse_recent_disconnects_10m"`
	SSEUpstreamErrorsTotal    uint64                   `json:"sse_upstream_errors_total"`
	LogQueueDepth             uint64                   `json:"log_queue_depth"`
	LogWrittenTotal           uint64                   `json:"log_written_total"`
	LogWriteBatchesTotal      uint64                   `json:"log_write_batches_total"`
	LogDroppedTotal           uint64                   `json:"log_dropped_total"`
	LogWriteFailuresTotal     uint64                   `json:"log_write_failures_total"`
	SlowDBOperationsTotal     uint64                   `json:"slow_db_operations_total"`
	Cleanup                   RuntimeCleanupMetricsOut `json:"cleanup"`
}

type SystemInfoOut struct {
	Service                       string                    `json:"service"`
	Version                       string                    `json:"version"`
	DefaultUpstreamTimeoutSeconds float64                   `json:"default_upstream_timeout_seconds"`
	UptimeSeconds                 uint64                    `json:"uptime_seconds"`
	CurrentServerTime             string                    `json:"current_server_time"`
	DatabaseOK                    bool                      `json:"database_ok"`
	DatabaseAllocatedBytes        *int64                    `json:"database_allocated_bytes,omitempty"`
	TotalLogCount                 int64                     `json:"total_log_count"`
	LogCount24h                   int64                     `json:"log_count_24h"`
	EnabledUpstreamCount          int64                     `json:"enabled_upstream_count"`
	TotalUpstreamCount            int64                     `json:"total_upstream_count"`
	RecentOneMinuteLogCount       int64                     `json:"recent_one_minute_log_count"`
	RuntimeLogSettings            RuntimeLogSettingsSummary `json:"runtime_log_settings"`
	RuntimeMetrics                RuntimeMetricsOut         `json:"runtime_metrics"`
}

const (
	DefaultLogBodyKeepCount                  int64 = 100
	DefaultLogRetentionDays                  int64 = 30
	DefaultLogBodyMaxBytes                   int64 = 200000
	DefaultMaxRetries                        int64 = 1
	DefaultSameUpstreamRetryIntervalMs       int64 = 1000
	DefaultAutoWeightFailurePenalty          int64 = 20
	DefaultAutoWeightSuccessIncrement        int64 = 5
	DefaultAutoWeightRecoveryIncrement       int64 = 10
	DefaultAutoWeightRecoveryIntervalSeconds int64 = 60
	// DefaultImageStorageMaxMB caps the directory generated images are saved
	// to. 10 GiB holds several thousand images at the 1–2 MiB they usually are.
	DefaultImageStorageMaxMB int64 = 10240
	// MaxImageStorageMaxMB is 1 TiB: a typo'd extra digit should be rejected,
	// not quietly let the directory fill the disk.
	MaxImageStorageMaxMB int64 = 1 << 20
	// MaxDashboardMultiplier bounds the display multiplier; beyond this it is
	// a typo, not a choice.
	MaxDashboardMultiplier float64 = 1000
	// MaxLogBodyKeepCount bounds how many recent logs keep their bodies. Bodies
	// are stored as deduplicated chunks, so a resent conversation costs only its
	// new turn and a long window stays affordable.
	MaxLogBodyKeepCount int64 = 100000
	// MaxLogBodyMaxBytes is 128 MiB, the largest response the proxy buffers.
	MaxLogBodyMaxBytes int64 = 128 << 20
)

// RuntimeSettings is the operator-editable policy stored in SQLite.
type RuntimeSettings struct {
	LogBodyKeepCount                  int64  `json:"log_body_keep_count"`
	LogRetentionDays                  int64  `json:"log_retention_days"`
	LogBodyMaxBytes                   int64  `json:"log_body_max_bytes"`
	MaxRetries                        int64  `json:"max_retries"`
	SameUpstreamRetryIntervalMs       int64  `json:"same_upstream_retry_interval_ms"`
	AutoWeightFailurePenalty          int64  `json:"auto_weight_failure_penalty"`
	AutoWeightSuccessIncrement        int64  `json:"auto_weight_success_increment"`
	AutoWeightRecoveryIncrement       int64  `json:"auto_weight_recovery_increment"`
	AutoWeightRecoveryIntervalSeconds int64  `json:"auto_weight_recovery_interval_seconds"`
	ProxyEnabled                      bool   `json:"proxy_enabled"`
	ProxyURL                          string `json:"proxy_url"`
	// DefaultUpstreamTimeoutSeconds is the fallback for channels that leave
	// their own timeout unset. Zero means "inherit the startup config", so an
	// operator who never touched it keeps whatever the deployment set.
	DefaultUpstreamTimeoutSeconds int64 `json:"default_upstream_timeout_seconds"`
	// ImageStorageMaxMB caps the saved-image directory; the oldest files go
	// first once it is exceeded. Zero turns saving off and empties it.
	ImageStorageMaxMB int64 `json:"image_storage_max_mb"`
	// DashboardMultiplier scales the request and token counts the dashboard
	// shows. Display only: logs, quotas and stored totals are untouched.
	DashboardMultiplier float64 `json:"dashboard_multiplier"`
	Revision            int64   `json:"revision"`
	UpdatedAt           string  `json:"updated_at"`
	// DatabaseOverride records that these values came from SQLite rather than
	// the startup defaults. It is not part of the stored row.
	DatabaseOverride bool `json:"-"`
}

// DefaultRuntimeSettings returns the safe startup policy.
func DefaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		LogBodyKeepCount:                  DefaultLogBodyKeepCount,
		LogRetentionDays:                  DefaultLogRetentionDays,
		LogBodyMaxBytes:                   DefaultLogBodyMaxBytes,
		MaxRetries:                        DefaultMaxRetries,
		SameUpstreamRetryIntervalMs:       DefaultSameUpstreamRetryIntervalMs,
		AutoWeightFailurePenalty:          DefaultAutoWeightFailurePenalty,
		AutoWeightSuccessIncrement:        DefaultAutoWeightSuccessIncrement,
		AutoWeightRecoveryIncrement:       DefaultAutoWeightRecoveryIncrement,
		AutoWeightRecoveryIntervalSeconds: DefaultAutoWeightRecoveryIntervalSeconds,
		ProxyEnabled:                      false,
		ProxyURL:                          "",
		DefaultUpstreamTimeoutSeconds:     0,
		ImageStorageMaxMB:                 DefaultImageStorageMaxMB,
		DashboardMultiplier:               1,
		Revision:                          0,
		UpdatedAt:                         "",
		DatabaseOverride:                  false,
	}
}

func (s *RuntimeSettings) Validate() error {
	for _, check := range []struct {
		value    int64
		min, max int64
		message  string
	}{
		{s.LogBodyKeepCount, 1, MaxLogBodyKeepCount, "log_body_keep_count must be between 1 and 100000"},
		{s.LogRetentionDays, 1, 3650, "log_retention_days must be between 1 and 3650"},
		{s.LogBodyMaxBytes, 0, MaxLogBodyMaxBytes, "log_body_max_bytes must be between 0 and 134217728"},
		{s.MaxRetries, 0, 5, "max_retries must be between 0 and 5"},
		{s.SameUpstreamRetryIntervalMs, 0, 60000, "same_upstream_retry_interval_ms must be between 0 and 60000"},
		{s.AutoWeightFailurePenalty, 0, 100, "auto_weight_failure_penalty must be between 0 and 100"},
		{s.AutoWeightSuccessIncrement, 0, 100, "auto_weight_success_increment must be between 0 and 100"},
		{s.AutoWeightRecoveryIncrement, 0, 100, "auto_weight_recovery_increment must be between 0 and 100"},
		{s.AutoWeightRecoveryIntervalSeconds, 1, 3600, "auto_weight_recovery_interval_seconds must be between 1 and 3600"},
		// 0 is "inherit"; anything set must be a sane forwarding timeout.
		{s.DefaultUpstreamTimeoutSeconds, 0, 3600, "default_upstream_timeout_seconds must be between 0 and 3600"},
		{s.ImageStorageMaxMB, 0, MaxImageStorageMaxMB, "image_storage_max_mb must be between 0 and 1048576"},
	} {
		if check.value < check.min || check.value > check.max {
			return ErrString(check.message)
		}
	}
	// NaN fails both comparisons, so test the accepted range, not the rejected one.
	if !(s.DashboardMultiplier > 0 && s.DashboardMultiplier <= MaxDashboardMultiplier) {
		return ErrString("dashboard_multiplier must be greater than 0 and at most 1000")
	}
	if err := validateProxyURL(s.ProxyURL, s.ProxyEnabled); err != nil {
		return err
	}
	return nil
}

// validateProxyURL accepts an empty value only when the proxy is disabled, and
// otherwise requires an absolute http/https/socks5 URL with a host.
func validateProxyURL(value string, enabled bool) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		if enabled {
			return ErrString("proxy_url is required when proxy_enabled is true")
		}
		return nil
	}
	if len(trimmed) > 500 {
		return ErrString("proxy_url must be at most 500 bytes")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ErrString("proxy_url must be a valid URL")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return ErrString("proxy_url scheme must be http, https, socks5, or socks5h")
	}
	if parsed.Host == "" {
		return ErrString("proxy_url must include a host")
	}
	return nil
}

type RuntimeSettingsIn struct {
	LogBodyKeepCount                  int64   `json:"log_body_keep_count"`
	LogRetentionDays                  int64   `json:"log_retention_days"`
	LogBodyMaxBytes                   int64   `json:"log_body_max_bytes"`
	MaxRetries                        int64   `json:"max_retries"`
	SameUpstreamRetryIntervalMs       int64   `json:"same_upstream_retry_interval_ms"`
	AutoWeightFailurePenalty          int64   `json:"auto_weight_failure_penalty"`
	AutoWeightSuccessIncrement        int64   `json:"auto_weight_success_increment"`
	AutoWeightRecoveryIncrement       int64   `json:"auto_weight_recovery_increment"`
	AutoWeightRecoveryIntervalSeconds int64   `json:"auto_weight_recovery_interval_seconds"`
	ProxyEnabled                      bool    `json:"proxy_enabled"`
	ProxyURL                          string  `json:"proxy_url"`
	DefaultUpstreamTimeoutSeconds     int64   `json:"default_upstream_timeout_seconds"`
	ImageStorageMaxMB                 int64   `json:"image_storage_max_mb"`
	DashboardMultiplier               float64 `json:"dashboard_multiplier"`
	Revision                          int64   `json:"revision"`
}

func (in *RuntimeSettingsIn) Validate() error {
	if in.Revision < 1 {
		return ErrString("revision must be at least 1")
	}
	candidate := DefaultRuntimeSettings()
	candidate.LogBodyKeepCount = in.LogBodyKeepCount
	candidate.LogRetentionDays = in.LogRetentionDays
	candidate.LogBodyMaxBytes = in.LogBodyMaxBytes
	candidate.MaxRetries = in.MaxRetries
	candidate.SameUpstreamRetryIntervalMs = in.SameUpstreamRetryIntervalMs
	candidate.AutoWeightFailurePenalty = in.AutoWeightFailurePenalty
	candidate.AutoWeightSuccessIncrement = in.AutoWeightSuccessIncrement
	candidate.AutoWeightRecoveryIncrement = in.AutoWeightRecoveryIncrement
	candidate.AutoWeightRecoveryIntervalSeconds = in.AutoWeightRecoveryIntervalSeconds
	candidate.ProxyEnabled = in.ProxyEnabled
	candidate.ProxyURL = in.ProxyURL
	candidate.DefaultUpstreamTimeoutSeconds = in.DefaultUpstreamTimeoutSeconds
	candidate.ImageStorageMaxMB = in.ImageStorageMaxMB
	candidate.DashboardMultiplier = in.DashboardMultiplier
	return candidate.Validate()
}

type RuntimeSettingsOut struct {
	LogBodyKeepCount                  int64   `json:"log_body_keep_count"`
	LogRetentionDays                  int64   `json:"log_retention_days"`
	LogBodyMaxBytes                   int64   `json:"log_body_max_bytes"`
	MaxRetries                        int64   `json:"max_retries"`
	SameUpstreamRetryIntervalMs       int64   `json:"same_upstream_retry_interval_ms"`
	AutoWeightFailurePenalty          int64   `json:"auto_weight_failure_penalty"`
	AutoWeightSuccessIncrement        int64   `json:"auto_weight_success_increment"`
	AutoWeightRecoveryIncrement       int64   `json:"auto_weight_recovery_increment"`
	AutoWeightRecoveryIntervalSeconds int64   `json:"auto_weight_recovery_interval_seconds"`
	ProxyEnabled                      bool    `json:"proxy_enabled"`
	ProxyURL                          string  `json:"proxy_url"`
	DefaultUpstreamTimeoutSeconds     int64   `json:"default_upstream_timeout_seconds"`
	ImageStorageMaxMB                 int64   `json:"image_storage_max_mb"`
	DashboardMultiplier               float64 `json:"dashboard_multiplier"`
	Revision                          int64   `json:"revision"`
	UpdatedAt                         string  `json:"updated_at"`
	DatabaseOverride                  bool    `json:"database_override"`
}

func NewRuntimeSettingsOut(s *RuntimeSettings) RuntimeSettingsOut {
	return RuntimeSettingsOut{
		LogBodyKeepCount:                  s.LogBodyKeepCount,
		LogRetentionDays:                  s.LogRetentionDays,
		LogBodyMaxBytes:                   s.LogBodyMaxBytes,
		MaxRetries:                        s.MaxRetries,
		SameUpstreamRetryIntervalMs:       s.SameUpstreamRetryIntervalMs,
		AutoWeightFailurePenalty:          s.AutoWeightFailurePenalty,
		AutoWeightSuccessIncrement:        s.AutoWeightSuccessIncrement,
		AutoWeightRecoveryIncrement:       s.AutoWeightRecoveryIncrement,
		AutoWeightRecoveryIntervalSeconds: s.AutoWeightRecoveryIntervalSeconds,
		ProxyEnabled:                      s.ProxyEnabled,
		ProxyURL:                          s.ProxyURL,
		DefaultUpstreamTimeoutSeconds:     s.DefaultUpstreamTimeoutSeconds,
		ImageStorageMaxMB:                 s.ImageStorageMaxMB,
		DashboardMultiplier:               s.DashboardMultiplier,
		Revision:                          s.Revision,
		UpdatedAt:                         s.UpdatedAt,
		DatabaseOverride:                  s.DatabaseOverride,
	}
}
