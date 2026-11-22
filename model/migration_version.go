package model

import (
	"fmt"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

// Versioned migration ledger.
//
// Why this exists (T4 fact table §1.2): the only migration mechanism is
// GORM AutoMigrate, which is one-way, has no version table and no down path.
// After an upgrade the database cannot say "which version am I on", so the only
// rollback that exists is restoring a backup and swapping the binary back — and
// a new binary started against a 32-bit quota schema refuses to boot outright
// (model/main.go ensureUserQuotaColumns).
//
// This file adds the missing ledger, additively:
//   - creates schema_migrations (version / description / applied_at / app version)
//   - lets code register ordered, idempotent migrations and records the ones
//     that ran, so an operator can answer "what has this database seen"
//   - records a baseline row for the pre-ledger AutoMigrate set
//
// It deliberately does NOT implement automatic down-migrations: dropping or
// rewriting columns is destructive and cannot be undone by re-running the
// upgrade. Rollback stays "restore RDS PITR/snapshot + previous binary", with the
// per-version down plan documented in out/P4-部署.md.

// SchemaMigration is the ledger row.
type SchemaMigration struct {
	Version     string `gorm:"primaryKey;type:varchar(64)" json:"version"`
	Description string `gorm:"type:varchar(255);not null" json:"description"`
	AppliedAt   int64  `gorm:"not null;index" json:"applied_at"`
	DurationMs  int64  `gorm:"not null;default:0" json:"duration_ms"`
	AppVersion  string `gorm:"type:varchar(64)" json:"app_version"`
	// Destructive marks migrations whose reversal needs a restore rather than a
	// script. Recorded so a rollback decision can be made from the ledger alone.
	Destructive bool `gorm:"not null;default:false" json:"destructive"`
}

func (SchemaMigration) TableName() string {
	return "schema_migrations"
}

// Migration is a versioned, idempotent upgrade step.
type Migration struct {
	Version string
	// Description is one line shown in the ledger; keep it operator-readable.
	Description string
	// Destructive=true means the step changed or dropped existing data shape, so
	// rolling back requires a database restore/snapshot rather than a down script.
	Destructive bool
	Up          func(*gorm.DB) error
}

var migrationRegistry []Migration

// RegisterMigration queues an upgrade step. Called from init() of the file that
// owns the change. Versions must be lexicographically sortable
// (YYYYMMDD-HHMM-<slug>) and unique.
func RegisterMigration(m Migration) {
	if m.Version == "" || m.Up == nil {
		panic("model: registered migration must have Version and Up")
	}
	for _, existing := range migrationRegistry {
		if existing.Version == m.Version {
			panic(fmt.Sprintf("model: duplicate migration version %s", m.Version))
		}
	}
	migrationRegistry = append(migrationRegistry, m)
	sort.Slice(migrationRegistry, func(i, j int) bool {
		return migrationRegistry[i].Version < migrationRegistry[j].Version
	})
}

// ensureMigrationVersionTable creates the ledger itself. Must run before any
// other migration so the table exists on both fresh and existing databases.
func ensureMigrationVersionTable(db *gorm.DB) error {
	return db.AutoMigrate(&SchemaMigration{})
}

// RunPendingMigrations executes every registered migration that is not in the
// ledger yet, in version order, and records each successful step.
// A failing step aborts the remaining ones and returns the error so startup can
// refuse to continue (same contract as the rest of migrateDB).
func RunPendingMigrations(db *gorm.DB) error {
	if len(migrationRegistry) == 0 {
		return nil
	}
	applied := make(map[string]bool, len(migrationRegistry))
	var rows []SchemaMigration
	if err := db.Find(&rows).Error; err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for _, row := range rows {
		applied[row.Version] = true
	}
	for _, m := range migrationRegistry {
		if applied[m.Version] {
			continue
		}
		common.SysLog(common.LogText("applying migration %s (%s)", m.Version, m.Description))
		start := time.Now()
		if err := m.Up(db); err != nil {
			common.SysError(common.LogText("migration %s failed: %s", m.Version, err.Error()))
			return fmt.Errorf("migration %s (%s) failed: %w", m.Version, m.Description, err)
		}
		row := SchemaMigration{
			Version:     m.Version,
			Description: m.Description,
			AppliedAt:   common.GetTimestamp(),
			DurationMs:  time.Since(start).Milliseconds(),
			AppVersion:  common.Version,
			Destructive: m.Destructive,
		}
		if err := db.Create(&row).Error; err != nil {
			return fmt.Errorf("record migration %s: %w", m.Version, err)
		}
	}
	return nil
}

// legacyBaselineVersion labels the pre-ledger schema produced by AutoMigrate on
// the 0.1/0.2 code line. Recorded once so the ledger is never an empty table on
// databases that predate it.
const legacyBaselineVersion = "20261122-0000-legacy-automigrate-baseline"

// RecordLegacyBaseline inserts the baseline row when the ledger is empty.
// Idempotent: a second start is a no-op.
func RecordLegacyBaseline(db *gorm.DB) error {
	var count int64
	if err := db.Model(&SchemaMigration{}).Count(&count).Error; err != nil {
		return fmt.Errorf("count schema_migrations: %w", err)
	}
	if count > 0 {
		return nil
	}
	row := SchemaMigration{
		Version:     legacyBaselineVersion,
		Description: "GORM AutoMigrate baseline (37 models) captured when the migration ledger was introduced",
		AppliedAt:   common.GetTimestamp(),
		AppVersion:  common.Version,
		Destructive: true,
	}
	if err := db.Create(&row).Error; err != nil {
		return fmt.Errorf("record legacy baseline: %w", err)
	}
	common.SysLog(common.LogText("recorded schema baseline %s", legacyBaselineVersion))
	return nil
}

// AppliedMigrations returns the ledger, newest first. Read-only helper used by
// operators and by future admin diagnostics.
func AppliedMigrations(db *gorm.DB) ([]SchemaMigration, error) {
	var rows []SchemaMigration
	if err := db.Order("applied_at desc, version desc").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CurrentSchemaVersion returns the highest applied version, or "" when the
// ledger has no rows yet.
func CurrentSchemaVersion(db *gorm.DB) (string, error) {
	var row SchemaMigration
	err := db.Order("version desc").Limit(1).Find(&row).Error
	if err != nil {
		return "", err
	}
	return row.Version, nil
}
