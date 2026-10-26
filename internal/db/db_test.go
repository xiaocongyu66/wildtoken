package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/liguangsheng/wildtoken/internal/apperr"
	"github.com/liguangsheng/wildtoken/internal/models"
)

// memoryDB returns an initialized in-memory database. A shared cache keeps the
// single logical database alive across pooled connections.
func memoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := Init(context.Background(), db); err != nil {
		t.Fatalf("init: %v", err)
	}
	return db
}

func TestInitIsIdempotent(t *testing.T) {
	db := memoryDB(t)
	if err := Init(context.Background(), db); err != nil {
		t.Fatalf("second init: %v", err)
	}

	for _, table := range []string{
		"upstreams", "api_tokens", "request_logs",
		"log_metas", "log_chunks", "request_log_snapshots", "request_log_bodies",
		"runtime_settings", "admin_credential",
		"model_test_prompt_templates",
	} {
		var count int64
		err := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).
			Scan(&count)
		if err != nil {
			t.Fatalf("lookup %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s was not created", table)
		}
	}
}

func TestSeededDefaultsMatchTheRustSchema(t *testing.T) {
	db := memoryDB(t)

	var settings models.RuntimeSettings
	var ok bool
	var err error
	if settings, ok, err = LoadRuntimeSettings(context.Background(), db); err != nil || !ok {
		t.Fatalf("load runtime settings: %v (ok=%v)", err, ok)
	}
	if settings.LogBodyKeepCount != 100 || settings.LogRetentionDays != 30 ||
		settings.LogBodyMaxBytes != 200000 || settings.Revision != 1 {
		t.Errorf("unexpected seeded runtime settings: %+v", settings)
	}

	prompts, err := ListModelTestPromptTemplates(context.Background(), db)
	if err != nil {
		t.Fatalf("list prompt templates: %v", err)
	}
	if len(prompts) != 10 {
		t.Errorf("seeded %d prompt templates, want 10", len(prompts))
	}
}

func TestUpdateRuntimeSettingsUsesRevisionCompareAndSwap(t *testing.T) {
	db := memoryDB(t)
	input := models.RuntimeSettingsIn{
		LogBodyKeepCount:                  99,
		LogRetentionDays:                  30,
		LogBodyMaxBytes:                   200000,
		MaxRetries:                        2,
		SameUpstreamRetryIntervalMs:       2500,
		AutoWeightFailurePenalty:          25,
		AutoWeightSuccessIncrement:        8,
		AutoWeightRecoveryIncrement:       12,
		AutoWeightRecoveryIntervalSeconds: 90,
		DashboardMultiplier:               1.5,
		Revision:                          1,
	}

	updated, err := UpdateRuntimeSettings(context.Background(), db, &input)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if updated.Revision != 2 || updated.MaxRetries != 2 ||
		updated.SameUpstreamRetryIntervalMs != 2500 ||
		updated.AutoWeightFailurePenalty != 25 ||
		updated.AutoWeightSuccessIncrement != 8 ||
		updated.AutoWeightRecoveryIncrement != 12 ||
		updated.AutoWeightRecoveryIntervalSeconds != 90 ||
		updated.DashboardMultiplier != 1.5 {
		t.Errorf("unexpected updated settings: %+v", updated)
	}

	// Replaying the same revision must lose the compare-and-swap.
	if _, err := UpdateRuntimeSettings(context.Background(), db, &input); err == nil {
		t.Error("stale revision was accepted")
	}
}

func TestCredentialBootstrapPreservesExistingAndRotationIncrementsVersion(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	first, err := BootstrapAdminCredential(ctx, db, "existing-hash")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if first.CredentialVersion != 1 {
		t.Errorf("first version = %d, want 1", first.CredentialVersion)
	}

	again, err := BootstrapAdminCredential(ctx, db, "replacement-bootstrap-hash")
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if again.CredentialHash != "existing-hash" {
		t.Errorf("bootstrap overwrote an existing credential: %q", again.CredentialHash)
	}

	rotated, ok, err := RotateAdminCredential(ctx, db, "rotated-hash", 1)
	if err != nil || !ok {
		t.Fatalf("rotate: %v (ok=%v)", err, ok)
	}
	if rotated.CredentialHash != "rotated-hash" || rotated.CredentialVersion != 2 {
		t.Errorf("unexpected rotation result: %+v", rotated)
	}

	// A replayed version is a compare-and-swap miss, not an error.
	if _, ok, err := RotateAdminCredential(ctx, db, "third-hash", 1); err != nil || ok {
		t.Errorf("stale rotation: ok=%v err=%v", ok, err)
	}
}

func TestTokenPreviewNeverContainsAShortTokenInFull(t *testing.T) {
	for _, testCase := range []struct{ token, want string }{
		{"", "…"},
		{"x", "…"},
		// A quarter, not a half: half of a five-character token is most of it.
		{"short", "s…"},
		{"long-enough-token", "long…"},
	} {
		if got := TokenPreview(testCase.token); got != testCase.want {
			t.Errorf("TokenPreview(%q) = %q, want %q", testCase.token, got, testCase.want)
		}
	}
}

func TestGeneratedTokensCarryTheStandardShapeAndDoNotRepeat(t *testing.T) {
	first, err := GenerateAPIToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, err := GenerateAPIToken()
	if err != nil {
		t.Fatalf("generate again: %v", err)
	}

	for _, testCase := range []struct{ label, token string }{
		{"first", first},
		{"second", second},
	} {
		if !strings.HasPrefix(testCase.token, "sk-") {
			t.Errorf("%s token %q does not carry the sk- prefix", testCase.label, testCase.token)
		}
		if len(testCase.token) != 35 {
			t.Errorf("%s token %q has length %d, want 35",
				testCase.label, testCase.token, len(testCase.token))
		}
		for _, char := range strings.TrimPrefix(testCase.token, "sk-") {
			alphanumeric := (char >= 'a' && char <= 'z') ||
				(char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
			if !alphanumeric {
				t.Errorf("%s token %q contains %q, want only [A-Za-z0-9]",
					testCase.label, testCase.token, char)
			}
		}
	}

	if first == second {
		t.Errorf("two consecutive tokens are identical: %q", first)
	}
}

func TestTheStoredPlaintextIsHandedBackByListAndGet(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	created, err := CreateToken(ctx, db, &models.APITokenIn{Name: "copyable", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Token == "" {
		t.Fatal("creation returned no plaintext")
	}

	fetched, ok, err := GetToken(ctx, db, created.ID)
	if err != nil || !ok {
		t.Fatalf("get: %v (ok=%v)", err, ok)
	}
	listed, err := ListTokens(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d tokens, want 1", len(listed))
	}

	for _, testCase := range []struct{ label, token string }{
		{"GetToken", fetched.Token},
		{"ListTokens", listed[0].Token},
	} {
		if testCase.token != created.Token {
			t.Errorf("%s returned token %q, want %q",
				testCase.label, testCase.token, created.Token)
		}
	}
}

func TestRowsPredatingPlaintextStorageReportAnEmptyToken(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	// A row as an older WildToken wrote it: digest and preview only.
	digest := TokenDigest("legacy-plaintext")
	result, err := db.Exec(
		`INSERT INTO api_tokens (name, token, token_hash, token_preview)
         VALUES ('legacy', ?, ?, ?)`,
		digest, digest, TokenPreview("legacy-plaintext"))
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}

	fetched, ok, err := GetToken(ctx, db, id)
	if err != nil || !ok {
		t.Fatalf("get: %v (ok=%v)", err, ok)
	}
	if fetched.Token != "" {
		t.Errorf("unrecoverable token read as %q, want the empty string", fetched.Token)
	}

	listed, err := ListTokens(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Token != "" {
		t.Errorf("list returned %+v, want one row with an empty token", listed)
	}
}

// A database created before token_plain existed must gain the column on the
// next startup, with its rows left readable rather than failing the query.
func TestAnOlderDatabaseGainsThePlaintextColumn(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	digest := TokenDigest("legacy-plaintext")
	if _, err := db.Exec(
		`INSERT INTO api_tokens (name, token, token_hash, token_preview)
         VALUES ('legacy', ?, ?, '…')`, digest, digest); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE api_tokens DROP COLUMN token_plain"); err != nil {
		t.Fatalf("simulate the older schema: %v", err)
	}

	if err := MigrateLegacyTokenStorage(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var columns int64
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info('api_tokens') WHERE name = 'token_plain'").
		Scan(&columns); err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	if columns != 1 {
		t.Fatal("token_plain was not added to the upgraded table")
	}

	listed, err := ListTokens(ctx, db)
	if err != nil {
		t.Fatalf("list after upgrade: %v", err)
	}
	if len(listed) != 1 || listed[0].Token != "" {
		t.Errorf("upgraded rows read as %+v, want one row with an empty token", listed)
	}

	// Tokens minted after the upgrade are copyable.
	created, err := CreateToken(ctx, db, &models.APITokenIn{Name: "fresh", Enabled: true})
	if err != nil {
		t.Fatalf("create after upgrade: %v", err)
	}
	fetched, ok, err := GetToken(ctx, db, created.ID)
	if err != nil || !ok {
		t.Fatalf("get after upgrade: %v (ok=%v)", err, ok)
	}
	if fetched.Token != created.Token {
		t.Errorf("token after upgrade = %q, want %q", fetched.Token, created.Token)
	}
}

// Repair runs whenever a row lacks a digest. Only those rows hold plaintext: a
// migrated row's token column holds its digest, and hashing that a second time
// broke every existing token at once.
func TestRepairingOneLegacyRowLeavesTheMigratedOnesAlone(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// The pre-hashing table, as an old database has it.
	if _, err := db.Exec(`CREATE TABLE api_tokens (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT NOT NULL UNIQUE,
        token TEXT NOT NULL UNIQUE,
        enabled INTEGER NOT NULL DEFAULT 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_tokens (name, token) VALUES ('first', 'first-plaintext')`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacyTokenStorage(ctx, db); err != nil {
		t.Fatalf("first migration: %v", err)
	}

	// A row that arrives without a digest, as an older binary writes one.
	if _, err := db.Exec(`INSERT INTO api_tokens (name, token) VALUES ('late', 'late-plaintext')`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacyTokenStorage(ctx, db); err != nil {
		t.Fatalf("repair: %v", err)
	}

	for name, plaintext := range map[string]string{"first": "first-plaintext", "late": "late-plaintext"} {
		var digest string
		if err := db.QueryRow("SELECT token_hash FROM api_tokens WHERE name = ?", name).
			Scan(&digest); err != nil {
			t.Fatal(err)
		}
		if digest != TokenDigest(plaintext) {
			t.Errorf("%s no longer authenticates: its digest was rewritten", name)
		}
	}
}

// An operator who deleted every starting template does not want them back at
// the next restart.
func TestDeletedPromptTemplatesStayDeleted(t *testing.T) {
	ctx := context.Background()
	db := memoryDB(t)
	if _, err := db.Exec("DELETE FROM model_test_prompt_templates"); err != nil {
		t.Fatal(err)
	}
	if err := Init(ctx, db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM model_test_prompt_templates").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("restart re-seeded %d deleted templates", count)
	}
}

// The legacy migration runs on every startup and rewrites the compatibility
// `token` column. It must leave token_plain alone, or a restart would silently
// strip every copyable credential.
func TestRerunningTheLegacyMigrationKeepsThePlaintext(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	created, err := CreateToken(ctx, db, &models.APITokenIn{Name: "survivor", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for round := 1; round <= 2; round++ {
		if err := MigrateLegacyTokenStorage(ctx, db); err != nil {
			t.Fatalf("migration round %d: %v", round, err)
		}
		fetched, ok, err := GetToken(ctx, db, created.ID)
		if err != nil || !ok {
			t.Fatalf("get after round %d: %v (ok=%v)", round, err, ok)
		}
		if fetched.Token != created.Token {
			t.Errorf("after round %d token = %q, want %q", round, fetched.Token, created.Token)
		}
	}

	// The compatibility column still holds the digest, not the plaintext.
	var legacyToken, storedPlain string
	err = db.QueryRow("SELECT token, token_plain FROM api_tokens WHERE id = ?", created.ID).
		Scan(&legacyToken, &storedPlain)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if legacyToken != TokenDigest(created.Token) {
		t.Errorf("legacy token column = %q, want the digest", legacyToken)
	}
	if storedPlain != created.Token {
		t.Errorf("token_plain = %q, want %q", storedPlain, created.Token)
	}
}

// None of the mutating paths name token_plain, so none of them may blank it.
func TestMutatingATokenNeverClearsThePlaintext(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	created, err := CreateToken(ctx, db, &models.APITokenIn{Name: "edited", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, testCase := range []struct {
		label  string
		mutate func() error
	}{
		{"UpdateToken", func() error {
			_, err := UpdateToken(ctx, db, created.ID,
				&models.APITokenUpdateIn{Name: "renamed", LimitExpression: "1M"})
			return err
		}},
		{"SetTokenEnabled", func() error {
			_, err := SetTokenEnabled(ctx, db, created.ID, false)
			return err
		}},
		{"ResetTokenUsage", func() error {
			_, err := ResetTokenUsage(ctx, db, created.ID)
			return err
		}},
	} {
		if err := testCase.mutate(); err != nil {
			t.Fatalf("%s: %v", testCase.label, err)
		}
		fetched, ok, err := GetToken(ctx, db, created.ID)
		if err != nil || !ok {
			t.Fatalf("get after %s: %v (ok=%v)", testCase.label, err, ok)
		}
		if fetched.Token != created.Token {
			t.Errorf("%s left token %q, want %q", testCase.label, fetched.Token, created.Token)
		}
	}
}

// Reopening the database file replays Init, which is the real restart path.
func TestPlaintextSurvivesReopeningTheDatabaseFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wildtoken.db")

	open := func() *sql.DB {
		t.Helper()
		database, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		database.SetMaxOpenConns(1)
		if err := Init(ctx, database); err != nil {
			t.Fatalf("init: %v", err)
		}
		return database
	}

	first := open()
	created, err := CreateToken(ctx, first, &models.APITokenIn{Name: "restarted", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := open()
	t.Cleanup(func() { second.Close() })
	reopened, ok, err := GetToken(ctx, second, created.ID)
	if err != nil || !ok {
		t.Fatalf("get after reopen: %v (ok=%v)", err, ok)
	}
	if reopened.Token != created.Token {
		t.Errorf("token after reopen = %q, want %q", reopened.Token, created.Token)
	}

	// Authentication still resolves the credential by digest alone.
	var authenticated int64
	err = second.QueryRow(
		"SELECT COUNT(*) FROM api_tokens WHERE token_hash = ? AND enabled = 1",
		TokenDigest(created.Token)).Scan(&authenticated)
	if err != nil {
		t.Fatalf("digest lookup: %v", err)
	}
	if authenticated != 1 {
		t.Errorf("digest lookup matched %d rows, want 1", authenticated)
	}
}

func TestCreationHonorsEnabledAndKeepsTheCompatibilityColumnsHashed(t *testing.T) {
	db := memoryDB(t)
	plaintext := "custom-token-value-1234"

	created, err := CreateToken(context.Background(), db, &models.APITokenIn{
		Name:        " disabled client ",
		Description: " disabled at creation ",
		Token:       &plaintext,
		Enabled:     false,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Token != plaintext {
		t.Errorf("created.Token = %q, want the plaintext", created.Token)
	}
	if created.Enabled {
		t.Error("enabled=false was not honored")
	}
	if created.Name != "disabled client" {
		t.Errorf("name = %q, want it trimmed", created.Name)
	}

	var storedToken, storedHash, storedPreview, storedPlain string
	var storedEnabled int64
	err = db.QueryRow(
		"SELECT token, token_hash, token_preview, token_plain, enabled FROM api_tokens WHERE id = ?",
		created.ID).Scan(&storedToken, &storedHash, &storedPreview, &storedPlain, &storedEnabled)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	digest := TokenDigest(plaintext)
	// The plaintext belongs in token_plain only: the startup migration rewrites
	// the compatibility column from token_hash on every boot.
	if storedToken != digest || storedHash != digest {
		t.Error("the compatibility columns hold something other than the digest")
	}
	if storedPlain != plaintext {
		t.Errorf("token_plain = %q, want the custom plaintext", storedPlain)
	}
	if storedPreview == plaintext {
		t.Error("preview stored the token in full")
	}
	if storedEnabled != 0 {
		t.Errorf("stored enabled = %d, want 0", storedEnabled)
	}
}

func TestCreationStoresANormalizedExpiryAndRefusesAPastOne(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	never, err := CreateToken(ctx, db, &models.APITokenIn{Name: "never", Enabled: true})
	if err != nil {
		t.Fatalf("create never-expiring: %v", err)
	}
	if never.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil", *never.ExpiresAt)
	}

	offset := "2099-01-01T08:00:00+08:00"
	dated, err := CreateToken(ctx, db, &models.APITokenIn{
		Name: "dated", Enabled: true, ExpiresAt: &offset,
	})
	if err != nil {
		t.Fatalf("create dated: %v", err)
	}
	if dated.ExpiresAt == nil || *dated.ExpiresAt != "2099-01-01 00:00:00" {
		t.Errorf("expires_at = %v, want the stored UTC shape", dated.ExpiresAt)
	}

	past := "2000-01-01 00:00:00"
	if _, err := CreateToken(ctx, db, &models.APITokenIn{
		Name: "past", Enabled: true, ExpiresAt: &past,
	}); err == nil {
		t.Error("a past expiry was accepted")
	}
}

// tokenColumnsOf reads the four columns a credential lives in, so a test can
// tell an edit that rewrote them from one that left them alone.
func tokenColumnsOf(t *testing.T, db *sql.DB, id int64) [4]string {
	t.Helper()
	var columns [4]string
	err := db.QueryRow(
		"SELECT token, token_hash, token_preview, COALESCE(token_plain, '') FROM api_tokens WHERE id = ?",
		id).Scan(&columns[0], &columns[1], &columns[2], &columns[3])
	if err != nil {
		t.Fatalf("read token columns: %v", err)
	}
	return columns
}

// Replacing the value must retire the old one at once. Authentication resolves
// token_hash per request with nothing cached, so the digest lookups below are
// exactly what the proxy does.
func TestReplacingATokenValueRetiresTheOldOneImmediately(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	created, err := CreateToken(ctx, db, &models.APITokenIn{Name: "rotating", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	replacement := "sk-rotated-by-the-operator-0001"
	updated, err := UpdateToken(ctx, db, created.ID, &models.APITokenUpdateIn{
		Name: "rotating", Token: &replacement,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Token != replacement {
		t.Errorf("update returned token %q, want %q", updated.Token, replacement)
	}
	if updated.TokenPreview != TokenPreview(replacement) {
		t.Errorf("preview = %q, want %q", updated.TokenPreview, TokenPreview(replacement))
	}

	fetched, ok, err := GetToken(ctx, db, created.ID)
	if err != nil || !ok {
		t.Fatalf("get: %v (ok=%v)", err, ok)
	}
	if fetched.Token != replacement {
		t.Errorf("stored token = %q, want %q", fetched.Token, replacement)
	}

	for _, testCase := range []struct {
		label string
		token string
		want  int64
	}{
		{"the new value", replacement, 1},
		{"the retired value", created.Token, 0},
	} {
		var matches int64
		if err := db.QueryRow("SELECT COUNT(*) FROM api_tokens WHERE token_hash = ?",
			TokenDigest(testCase.token)).Scan(&matches); err != nil {
			t.Fatalf("digest lookup for %s: %v", testCase.label, err)
		}
		if matches != testCase.want {
			t.Errorf("authenticating with %s matched %d rows, want %d",
				testCase.label, matches, testCase.want)
		}
	}

	// The compatibility column tracks the new digest, as it does on creation.
	columns := tokenColumnsOf(t, db, created.ID)
	digest := TokenDigest(replacement)
	if columns[0] != digest || columns[1] != digest || columns[3] != replacement {
		t.Errorf("stored columns = %v, want the new digest and plaintext", columns)
	}
}

// The console echoes the current value back on every save.
func TestResubmittingTheSameTokenIsNotAChange(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	created, err := CreateToken(ctx, db, &models.APITokenIn{Name: "echoed", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before := tokenColumnsOf(t, db, created.ID)

	unchanged := created.Token
	updated, err := UpdateToken(ctx, db, created.ID, &models.APITokenUpdateIn{
		Name: "echoed and renamed", Token: &unchanged,
	})
	if err != nil {
		t.Fatalf("resubmitting the same token was rejected: %v", err)
	}
	if updated.Token != created.Token {
		t.Errorf("token = %q, want it unchanged at %q", updated.Token, created.Token)
	}
	if updated.Name != "echoed and renamed" {
		t.Errorf("name = %q, want the edit applied", updated.Name)
	}
	if after := tokenColumnsOf(t, db, created.ID); after != before {
		t.Errorf("token columns changed from %v to %v", before, after)
	}
}

func TestAnAbsentOrBlankTokenLeavesTheCredentialAlone(t *testing.T) {
	blank := ""
	for _, testCase := range []struct {
		label string
		token *string
	}{
		{"absent", nil},
		{"blank", &blank},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			db := memoryDB(t)
			ctx := context.Background()

			created, err := CreateToken(ctx, db, &models.APITokenIn{Name: "kept", Enabled: true})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			before := tokenColumnsOf(t, db, created.ID)

			updated, err := UpdateToken(ctx, db, created.ID, &models.APITokenUpdateIn{
				Name: "kept and renamed", Description: "edited", Token: testCase.token,
			})
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if updated.Token != created.Token {
				t.Errorf("token = %q, want it unchanged at %q", updated.Token, created.Token)
			}
			if updated.Name != "kept and renamed" || updated.Description != "edited" {
				t.Errorf("other fields were not updated: %+v", updated)
			}
			if after := tokenColumnsOf(t, db, created.ID); after != before {
				t.Errorf("token columns changed from %v to %v", before, after)
			}
		})
	}
}

// openContendedDB returns a file database with a real pool and the connection
// parameters the server builds, which is what makes two writers able to overlap.
func openContendedDB(t *testing.T, txLock string) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wildtoken.db")
	database, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=busy_timeout(5000)&_txlock="+txLock)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	database.SetMaxOpenConns(4)
	if err := Init(context.Background(), database); err != nil {
		t.Fatalf("init: %v", err)
	}
	return database
}

// SQLiteTxLock is what makes a read-then-write transaction safe, and nothing in
// the store would fail visibly if the DSN lost it — so it is asserted here.
//
// Under deferred locking a transaction that reads before it writes holds a
// snapshot another commit can invalidate, and the write then fails with
// SQLITE_BUSY_SNAPSHOT, which busy_timeout cannot wait out. That surfaces as a
// database error rather than the collision report the store means to send.
func TestTheConfiguredTxLockSerializesAReadThenWriteTransaction(t *testing.T) {
	ctx := context.Background()
	database := openContendedDB(t, SQLiteTxLock)

	created, err := CreateToken(ctx, database, &models.APITokenIn{Name: "contended", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Hold a transaction that has read but not yet written.
	holder, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback()
	if _, _, err := GetToken(ctx, holder, created.ID); err != nil {
		t.Fatalf("holder read: %v", err)
	}

	// A second edit must wait for the holder instead of invalidating itself
	// against it. With deferred locking this is the interleaving that fails.
	competing := make(chan error, 1)
	go func() {
		_, err := UpdateToken(ctx, database, created.ID,
			&models.APITokenUpdateIn{Name: "renamed by the competitor"})
		competing <- err
	}()

	select {
	case err := <-competing:
		t.Fatalf("the competing edit did not wait for the open transaction: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	if _, err := holder.ExecContext(ctx,
		"UPDATE api_tokens SET description = 'held' WHERE id = ?", created.ID); err != nil {
		t.Fatalf("holder write: %v", err)
	}
	if err := holder.Commit(); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	if err := <-competing; err != nil {
		t.Fatalf("the competing edit failed instead of waiting its turn: %v", err)
	}
	reloaded, ok, err := GetToken(ctx, database, created.ID)
	if err != nil || !ok {
		t.Fatalf("reload: %v (ok=%v)", err, ok)
	}
	if reloaded.Name != "renamed by the competitor" {
		t.Errorf("name = %q, want the second edit applied", reloaded.Name)
	}
}

// Two operators claiming one value at once must not both win, and the loser must
// get the same business error a sequential collision produces. This needs a file
// database with a real connection pool: the shared-cache helper serializes
// everything onto one connection and would never let the two overlap.
func TestTwoEditsClaimingOneValueLeaveExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	database := openContendedDB(t, SQLiteTxLock)

	first, err := CreateToken(ctx, database, &models.APITokenIn{Name: "first", Enabled: true})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := CreateToken(ctx, database, &models.APITokenIn{Name: "second", Enabled: true})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	contested := "sk-both-operators-want-this-0001"
	var ready sync.WaitGroup
	ready.Add(2)
	release := make(chan struct{})
	results := make([]error, 2)

	var finished sync.WaitGroup
	for slot, target := range []models.APITokenCreatedOut{first, second} {
		finished.Add(1)
		go func(slot int, target models.APITokenCreatedOut) {
			defer finished.Done()
			value := contested
			ready.Done()
			<-release
			_, results[slot] = UpdateToken(ctx, database, target.ID,
				&models.APITokenUpdateIn{Name: target.Name, Token: &value})
		}(slot, target)
	}
	ready.Wait()
	close(release)
	finished.Wait()

	winners, losers := 0, 0
	for slot, err := range results {
		switch {
		case err == nil:
			winners++
		default:
			losers++
			var appErr *apperr.AppError
			if !errors.As(err, &appErr) {
				t.Errorf("goroutine %d failed with a non-AppError: %v", slot, err)
				continue
			}
			status, message := appErr.StatusAndMessage()
			if status != 400 {
				t.Errorf("goroutine %d got status %d (%q), want 400",
					slot, status, message)
			}
			if !strings.Contains(message, "already") {
				t.Errorf("goroutine %d got message %q, want a collision report",
					slot, message)
			}
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("%d winners and %d losers, want exactly one of each (results: %v)",
			winners, losers, results)
	}

	var holders int64
	if err := database.QueryRow("SELECT COUNT(*) FROM api_tokens WHERE token_hash = ?",
		TokenDigest(contested)).Scan(&holders); err != nil {
		t.Fatalf("count holders: %v", err)
	}
	if holders != 1 {
		t.Errorf("%d rows hold the contested value, want 1", holders)
	}
}

func TestTakingAValueAnotherTokenOwnsIsReportedAsABadRequest(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	first, err := CreateToken(ctx, db, &models.APITokenIn{Name: "first", Enabled: true})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := CreateToken(ctx, db, &models.APITokenIn{Name: "second", Enabled: true})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	before := tokenColumnsOf(t, db, second.ID)

	taken := first.Token
	_, err = UpdateToken(ctx, db, second.ID, &models.APITokenUpdateIn{
		Name: "second", Token: &taken,
	})
	if err == nil {
		t.Fatal("a value another token owns was accepted")
	}

	// A raw constraint failure would surface as a 500 with a generic message.
	var appErr *apperr.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error %v is not an AppError", err)
	}
	status, message := appErr.StatusAndMessage()
	if status != 400 {
		t.Errorf("status = %d, want 400", status)
	}
	if !strings.Contains(message, "already used by another token") {
		t.Errorf("message = %q, want it to name the collision", message)
	}

	if after := tokenColumnsOf(t, db, second.ID); after != before {
		t.Errorf("the refused edit still wrote: %v became %v", before, after)
	}
}

func TestInvalidReplacementTokensAreRefused(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	created, err := CreateToken(ctx, db, &models.APITokenIn{Name: "guarded", Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before := tokenColumnsOf(t, db, created.ID)

	for _, testCase := range []struct{ label, token string }{
		{"a space", "sk-with a space"},
		{"a tab", "sk-with\ttab"},
		{"non-ASCII", "sk-令牌"},
		{"over 256 bytes", strings.Repeat("a", 257)},
	} {
		replacement := testCase.token
		if _, err := UpdateToken(ctx, db, created.ID, &models.APITokenUpdateIn{
			Name: "guarded", Token: &replacement,
		}); err == nil {
			t.Errorf("a token containing %s was accepted", testCase.label)
		}
	}

	if after := tokenColumnsOf(t, db, created.ID); after != before {
		t.Errorf("a refused value still wrote: %v became %v", before, after)
	}
}

func TestUpdatingCanRenewOrClearAnExpiryAndLeavesALapsedOneEditable(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	future := "2099-01-01 00:00:00"
	created, err := CreateToken(ctx, db, &models.APITokenIn{
		Name: "client", Enabled: true, ExpiresAt: &future,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Backdate the row the way time would have.
	if _, err := db.Exec(
		"UPDATE api_tokens SET expires_at = '2000-01-01 00:00:00' WHERE id = ?",
		created.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Renaming an expired token resends its own past expiry unchanged. That must
	// not be rejected, or the row could never be edited again.
	lapsed := "2000-01-01 00:00:00"
	renamed, err := UpdateToken(ctx, db, created.ID, &models.APITokenUpdateIn{
		Name: "renamed", ExpiresAt: &lapsed,
	})
	if err != nil {
		t.Fatalf("rename with unchanged lapsed expiry: %v", err)
	}
	if renamed.Name != "renamed" || renamed.ExpiresAt == nil || *renamed.ExpiresAt != lapsed {
		t.Errorf("unexpected rename result: %+v", renamed)
	}

	// Moving it to a different past instant is a real change, so it is not.
	otherPast := "2001-01-01 00:00:00"
	if _, err := UpdateToken(ctx, db, created.ID, &models.APITokenUpdateIn{
		Name: "renamed", ExpiresAt: &otherPast,
	}); err == nil {
		t.Error("backdating to a new past expiry was accepted")
	}

	renewal := "2099-06-01 00:00:00"
	renewed, err := UpdateToken(ctx, db, created.ID, &models.APITokenUpdateIn{
		Name: "renamed", ExpiresAt: &renewal,
	})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.ExpiresAt == nil || *renewed.ExpiresAt != renewal {
		t.Errorf("expires_at = %v, want %q", renewed.ExpiresAt, renewal)
	}

	// An absent expiry clears it — the update endpoint is a full replacement.
	if _, err := UpdateToken(ctx, db, created.ID, &models.APITokenUpdateIn{
		Name: "renamed",
	}); err != nil {
		t.Fatalf("clear expiry: %v", err)
	}
	cleared, ok, err := GetToken(ctx, db, created.ID)
	if err != nil || !ok {
		t.Fatalf("reload: %v (ok=%v)", err, ok)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil", *cleared.ExpiresAt)
	}
}

func TestUpstreamRoundTripsItsJSONColumns(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	input := models.DefaultUpstreamIn()
	input.Name = "primary"
	input.BaseURL = "https://api.example.com"
	input.ModelNames = []string{"gpt-4o", "gpt-4o-mini"}
	input.ModelPrefixes = []string{"gpt-"}
	input.ModelMappings = map[string]string{"alias": "gpt-4o"}
	input.EffortMappings = map[string]string{"max": "xhigh"}
	input.ExtraHeaders = map[string]string{"x-tenant": "acme"}

	created, err := CreateUpstream(ctx, db, &input)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 0 is the service default, resolved per request.
	if created.TimeoutSeconds != 0 {
		t.Errorf("timeout = %v, want 0 for the service default", created.TimeoutSeconds)
	}
	if len(created.ModelNames) != 2 || created.ModelMappings["alias"] != "gpt-4o" ||
		created.ExtraHeaders["x-tenant"] != "acme" ||
		created.EffortMappings["max"] != "xhigh" {
		t.Errorf("JSON columns did not round-trip: %+v", created)
	}
	if created.APIKeySet {
		t.Error("api_key_set is true for an upstream created without a key")
	}

	// An update without an api_key keeps the stored one; clear_api_key removes it.
	key := "sk-secret"
	update := models.UpstreamUpdate{UpstreamIn: input}
	update.APIKey = &key
	if _, err := UpdateUpstream(ctx, db, created.ID, &update); err != nil {
		t.Fatalf("update with key: %v", err)
	}

	update = models.UpstreamUpdate{UpstreamIn: input}
	kept, err := UpdateUpstream(ctx, db, created.ID, &update)
	if err != nil {
		t.Fatalf("update without key: %v", err)
	}
	if !kept.APIKeySet {
		t.Error("an absent api_key cleared the stored one")
	}

	update = models.UpstreamUpdate{UpstreamIn: input, ClearAPIKey: true}
	cleared, err := UpdateUpstream(ctx, db, created.ID, &update)
	if err != nil {
		t.Fatalf("clear key: %v", err)
	}
	if cleared.APIKeySet {
		t.Error("clear_api_key did not remove the stored key")
	}
}

func TestUpstreamRateLimitRoundTrips(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	input := models.DefaultUpstreamIn()
	input.Name = "limited"
	input.BaseURL = "https://api.example.com"
	raw := "  100/m  "
	input.RateLimit = &raw

	// The stored shape is the trimmed expression, so the console echoes back
	// exactly what the operator wrote.
	created, err := CreateUpstream(ctx, db, &input)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.RateLimit == nil || *created.RateLimit != "100/m" {
		t.Errorf("rate_limit = %v, want 100/m", created.RateLimit)
	}

	changed := "50/10s"
	update := models.UpstreamUpdate{UpstreamIn: input}
	update.RateLimit = &changed
	updated, err := UpdateUpstream(ctx, db, created.ID, &update)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.RateLimit == nil || *updated.RateLimit != "50/10s" {
		t.Errorf("rate_limit = %v, want 50/10s", updated.RateLimit)
	}

	// A blank expression stores NULL — the update endpoint is a full replacement.
	blank := "   "
	update.RateLimit = &blank
	cleared, err := UpdateUpstream(ctx, db, created.ID, &update)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.RateLimit != nil {
		t.Errorf("rate_limit = %q, want nil", *cleared.RateLimit)
	}

	// An invalid expression never reaches storage.
	invalid := "not-a-rate"
	update.RateLimit = &invalid
	if _, err := UpdateUpstream(ctx, db, created.ID, &update); err == nil {
		t.Error("an invalid rate limit expression was accepted")
	}
}

func TestDeletingAGroupRehomesTheChannelsItWasTheLastGroupOf(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	group, err := CreateGroup(ctx, db, &models.GroupIn{Name: "team-a"})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	// One channel serves only the doomed group; the other also serves default.
	only := models.DefaultUpstreamIn()
	only.Name = "only-in-team-a"
	only.BaseURL = "https://api.example.com"
	only.GroupIDs = []int64{group.ID}
	onlyCreated, err := CreateUpstream(ctx, db, &only)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	shared := models.DefaultUpstreamIn()
	shared.Name = "in-both"
	shared.BaseURL = "https://api.example.com"
	shared.GroupIDs = []int64{group.ID, models.DefaultGroupID}
	sharedCreated, err := CreateUpstream(ctx, db, &shared)
	if err != nil {
		t.Fatalf("create shared channel: %v", err)
	}

	if _, err := DeleteGroup(ctx, db, group.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}

	// The cascade alone left this channel in no group at all: still enabled,
	// still listed, and reachable by no token in the system.
	groups, err := ListUpstreamGroupIDs(ctx, db, onlyCreated.ID)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 1 || groups[0] != models.DefaultGroupID {
		t.Errorf("channel groups = %v, want the default group", groups)
	}

	// A channel that had somewhere else to go is left where it was.
	sharedGroups, err := ListUpstreamGroupIDs(ctx, db, sharedCreated.ID)
	if err != nil {
		t.Fatalf("list shared groups: %v", err)
	}
	if len(sharedGroups) != 1 || sharedGroups[0] != models.DefaultGroupID {
		t.Errorf("shared channel groups = %v, want only the default group", sharedGroups)
	}

	reachable, err := ListEnabledUpstreamsInGroup(ctx, db, models.DefaultGroupID)
	if err != nil {
		t.Fatalf("list reachable: %v", err)
	}
	if len(reachable) != 2 {
		t.Errorf("default group reaches %d channels, want 2", len(reachable))
	}
}

func TestCreatingATokenRejectsAGroupThatDoesNotExist(t *testing.T) {
	db := memoryDB(t)
	ctx := context.Background()

	missing := int64(4242)
	input := models.APITokenIn{Name: "caller", Enabled: true, GroupID: &missing}
	if _, err := CreateToken(ctx, db, &input); err == nil {
		t.Fatal("a token was created pointing at a group that does not exist")
	}

	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM api_tokens").Scan(&count); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if count != 0 {
		t.Errorf("%d tokens were left behind by a rejected create", count)
	}
}

func TestTokenPreviewNeverShowsMostOfAShortToken(t *testing.T) {
	// A generated token is 32 characters and still shows eight, which is what
	// tells two credentials apart in a list.
	if preview := TokenPreview("abcdefghijklmnopqrstuvwxyz012345"); preview != "abcdefgh…" {
		t.Errorf("preview = %q, want the first eight characters", preview)
	}

	// A custom one can be much shorter, and half of a short token is most of
	// it: nine characters used to reveal eight of them.
	for _, token := range []string{"a", "abcd", "abcdefgh", "abcdefghi", "abcdefghijklmnop"} {
		preview := TokenPreview(token)
		shown := len([]rune(preview)) - 1 // the ellipsis
		if shown*4 > len([]rune(token)) {
			t.Errorf("preview of a %d-character token showed %d of them",
				len([]rune(token)), shown)
		}
	}
}

func TestInitSurvivesAGroupsTableThatAlreadyUsesTheDefaultSlot(t *testing.T) {
	ctx := context.Background()

	// Either unique column can be the one already taken, and Init has to start
	// the service either way — it is the only thing that could let an operator
	// in to fix the row.
	for name, occupy := range map[string]string{
		"id 1 belongs to another group": `INSERT INTO groups (id, name, description)
             VALUES (1, 'not-default', '')`,
		"the default name has another id": `INSERT INTO groups (id, name, description)
             VALUES (7, 'default', '')`,
	} {
		t.Run(name, func(t *testing.T) {
			database, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			database.SetMaxOpenConns(1)
			t.Cleanup(func() { database.Close() })

			if _, err := database.ExecContext(ctx, createGroups); err != nil {
				t.Fatalf("create groups: %v", err)
			}
			if _, err := database.ExecContext(ctx, occupy); err != nil {
				t.Fatalf("occupy: %v", err)
			}

			if err := Init(ctx, database); err != nil {
				t.Fatalf("init refused to start: %v", err)
			}
		})
	}
}
