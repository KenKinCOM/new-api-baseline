// Command secret-migration moves upstream channel keys and credential options
// between plaintext and the `enc:v1:` envelope used at rest, and takes a
// verifiable backup before either direction.
//
// Typical use (see out/N5-密钥加密.md):
//
//	# 1. preview
//	SECRET_ENCRYPTION_KEY_FILE=/run/secrets/secret_encryption_key \
//	  secret-migration -mode=status
//
//	# 2. back up, then encrypt (single run, backup path is printed)
//	SECRET_ENCRYPTION_KEY_FILE=/run/secrets/secret_encryption_key \
//	  secret-migration -mode=encrypt -backup-dir=/var/backups/new-api
//
//	# 3. roll back to plaintext before downgrading the binary
//	SECRET_ENCRYPTION_KEY_FILE=/run/secrets/secret_encryption_key \
//	  secret-migration -mode=decrypt -backup-dir=/var/backups/new-api
//
// The tool never prints key material, never prints the DSN, and refuses to
// touch rows unless a backup of the affected tables exists and still matches
// the current row counts.
package main

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	_ "github.com/glebarez/go-sqlite"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	toolName    = "secret-migration"
	backupVer   = 1
	channelsTbl = "channels"
	optionsTbl  = "options"
)

// value is a driver-independent encoding of one column value, so a backup can
// be restored on the same dialect without guessing at Go types.
type value struct {
	T string  `json:"t"`
	S string  `json:"s,omitempty"`
	I int64   `json:"i,omitempty"`
	F float64 `json:"f,omitempty"`
	B bool    `json:"b,omitempty"`
	V []byte  `json:"v,omitempty"`
}

type backupRecord struct {
	Kind      string         `json:"kind"`
	Tool      string         `json:"tool,omitempty"`
	Version   int            `json:"version,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
	Dialect   string         `json:"dialect,omitempty"`
	DSNSHA256 string         `json:"dsn_sha256,omitempty"`
	Table     string         `json:"table,omitempty"`
	Columns   []string       `json:"columns,omitempty"`
	Counts    map[string]int `json:"counts,omitempty"`
	Values    []value        `json:"values,omitempty"`
}

type database struct {
	db      *sql.DB
	dialect string
	dsnHash string
	prefix  string
}

type stats struct {
	Scanned   int
	Changed   int
	Encrypted int
	Plain     int
	Empty     int
	Skipped   int
}

func main() {
	var (
		dsn        = flag.String("dsn", os.Getenv("SQL_DSN"), "database DSN (default: $SQL_DSN)")
		mode       = flag.String("mode", "status", "status | backup | encrypt | decrypt | restore")
		backupFile = flag.String("backup", "", "existing backup file to verify (encrypt/decrypt) or read (restore)")
		backupDir  = flag.String("backup-dir", "/var/backups/new-api", "directory for a fresh pre-change backup; empty string disables creating one")
		dryRun     = flag.Bool("dry-run", false, "report what would change without writing")
		prefix     = flag.String("table-prefix", "", "table prefix, when the deployment sets one")
	)
	flag.Parse()

	if err := run(*dsn, *mode, *backupFile, *backupDir, *prefix, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func run(dsn, mode, backupFile, backupDir, prefix string, dryRun bool) error {
	if strings.TrimSpace(dsn) == "" {
		return errors.New("no DSN: set SQL_DSN or pass -dsn (this tool must not guess the database)")
	}
	target, err := openDatabase(dsn, prefix)
	if err != nil {
		return err
	}
	defer target.db.Close()

	fmt.Printf("database: %s (dsn sha256 %.12s)\n", target.dialect, target.dsnHash)

	switch mode {
	case "status":
		return target.report()
	case "backup":
		path, counts, err := target.writeBackup(backupDir)
		if err != nil {
			return err
		}
		fmt.Printf("backup written: %s\n", path)
		fmt.Printf("row counts: %v\n", counts)
		return nil
	case "encrypt", "decrypt":
		return target.transform(mode, backupFile, backupDir, dryRun)
	case "restore":
		if backupFile == "" {
			return errors.New("-mode=restore requires -backup=<file>")
		}
		return target.restore(backupFile, dryRun)
	default:
		return fmt.Errorf("unknown -mode=%q (want status|backup|encrypt|decrypt|restore)", mode)
	}
}

func openDatabase(dsn, prefix string) (*database, error) {
	driver, name, err := resolveDriver(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, name)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	sum := sha256.Sum256([]byte(dsn))
	return &database{db: db, dialect: driver, dsnHash: hex.EncodeToString(sum[:]), prefix: prefix}, nil
}

// resolveDriver maps a DSN to a database/sql driver name and connection string.
// Anything that is not obviously PostgreSQL is treated as a SQLite file when it
// carries no "@" (which every MySQL DSN does), so a bare path works too.
func resolveDriver(dsn string) (driver, name string, err error) {
	trimmed := strings.TrimSpace(dsn)
	switch {
	case strings.HasPrefix(trimmed, "postgres://"), strings.HasPrefix(trimmed, "postgresql://"):
		return "pgx", trimmed, nil
	case strings.HasPrefix(trimmed, "mysql:"):
		return "mysql", strings.TrimPrefix(trimmed, "mysql:"), nil
	case strings.HasPrefix(trimmed, "sqlite:"):
		return "sqlite", strings.TrimPrefix(trimmed, "sqlite:"), nil
	case strings.HasPrefix(trimmed, "local"):
		return "sqlite", common.SQLitePath, nil
	case strings.Contains(trimmed, "@"):
		return "mysql", trimmed, nil
	case strings.HasSuffix(trimmed, ".db"), strings.HasSuffix(trimmed, ".sqlite"), strings.HasSuffix(trimmed, ".sqlite3"):
		return "sqlite", trimmed, nil
	}
	return "", "", fmt.Errorf("cannot infer the database driver from the DSN; use mysql:<dsn>, sqlite:<path>, or postgres://...")
}

func (d *database) quote(ident string) string {
	if d.dialect == "mysql" {
		return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func (d *database) table(name string) string {
	return d.quote(d.prefix + name)
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func (d *database) report() error {
	channels, err := d.scanChannelKeys()
	if err != nil {
		return err
	}
	options, err := d.scanOptionValues()
	if err != nil {
		return err
	}
	printStats("channels.key", channels)
	printStats("options.value (sensitive keys)", options)

	fmt.Printf("encryption key configured: %v (origin: %s)\n", common.SecretEncryptionEnabled(), common.SecretEncryptionKeyOrigin())
	if channels.Plain > 0 || options.Plain > 0 {
		fmt.Println("next step: -mode=encrypt -backup-dir=<dir>")
	}
	return nil
}

func printStats(label string, s stats) {
	fmt.Printf("%-32s rows=%-6d encrypted=%-6d plaintext=%-6d empty=%d\n", label, s.Scanned, s.Encrypted, s.Plain, s.Empty)
}

type row struct {
	id     string // primary key as text
	secret string
}

func (d *database) scanChannelKeys() (stats, error) {
	rows, err := d.db.Query(fmt.Sprintf("SELECT id, %s FROM %s", d.quote("key"), d.table(channelsTbl)))
	if err != nil {
		return stats{}, fmt.Errorf("read channels: %w", err)
	}
	defer rows.Close()
	return classify(rows)
}

func (d *database) scanOptionValues() (stats, error) {
	rows, err := d.db.Query(fmt.Sprintf("SELECT %s, %s FROM %s", d.quote("key"), d.quote("value"), d.table(optionsTbl)))
	if err != nil {
		return stats{}, fmt.Errorf("read options: %w", err)
	}
	defer rows.Close()

	var out stats
	for rows.Next() {
		var key, val sql.NullString
		if err := rows.Scan(&key, &val); err != nil {
			return stats{}, err
		}
		if !model.IsSensitiveOption(key.String) {
			continue
		}
		out.Scanned++
		switch {
		case !val.Valid || val.String == "":
			out.Empty++
		case common.IsEncryptedSecret(val.String):
			out.Encrypted++
		default:
			out.Plain++
		}
	}
	return out, rows.Err()
}

func classify(rows *sql.Rows) (stats, error) {
	var out stats
	for rows.Next() {
		var id, secret sql.NullString
		if err := rows.Scan(&id, &secret); err != nil {
			return stats{}, err
		}
		out.Scanned++
		switch {
		case !secret.Valid || secret.String == "":
			out.Empty++
		case common.IsEncryptedSecret(secret.String):
			out.Encrypted++
		default:
			out.Plain++
		}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// backup
// ---------------------------------------------------------------------------

func (d *database) writeBackup(dir string) (string, map[string]int, error) {
	if strings.TrimSpace(dir) == "" {
		return "", nil, errors.New("backup directory is empty; pass -backup-dir or -backup")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create backup dir: %w", err)
	}
	stamp := time.Now().Format("20060102T150405.000000000")
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.jsonl.gz", toolName, strings.ReplaceAll(stamp, ".", "-")))

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("create backup file: %w", err)
	}
	gz := gzip.NewWriter(file)
	buf := bufio.NewWriter(gz)
	enc := json.NewEncoder(buf)

	fail := func(err error) (string, map[string]int, error) {
		file.Close()
		os.Remove(path)
		return "", nil, err
	}

	header := backupRecord{
		Kind:      "header",
		Tool:      toolName,
		Version:   backupVer,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Dialect:   d.dialect,
		DSNSHA256: d.dsnHash,
	}
	if err := enc.Encode(header); err != nil {
		return fail(err)
	}

	counts := map[string]int{}
	for _, table := range []string{channelsTbl, optionsTbl} {
		columns, values, err := d.dumpTable(table)
		if err != nil {
			return fail(err)
		}
		if err := enc.Encode(backupRecord{Kind: "table", Table: table, Columns: columns}); err != nil {
			return fail(err)
		}
		for _, rowValues := range values {
			if err := enc.Encode(backupRecord{Kind: "row", Table: table, Values: rowValues}); err != nil {
				return fail(err)
			}
		}
		counts[table] = len(values)
	}
	if err := enc.Encode(backupRecord{Kind: "footer", Counts: counts}); err != nil {
		return fail(err)
	}
	if err := buf.Flush(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		return "", nil, fmt.Errorf("close backup file: %w", err)
	}
	return path, counts, nil
}

func (d *database) dumpTable(table string) ([]string, [][]value, error) {
	rows, err := d.db.Query("SELECT * FROM " + d.table(table))
	if err != nil {
		return nil, nil, fmt.Errorf("dump %s: %w", table, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out [][]value
	for rows.Next() {
		cells := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		encoded := make([]value, len(columns))
		for i, cell := range cells {
			encoded[i] = encodeValue(cell)
		}
		out = append(out, encoded)
	}
	return columns, out, rows.Err()
}

func encodeValue(v any) value {
	switch typed := v.(type) {
	case nil:
		return value{T: "null"}
	case string:
		return value{T: "str", S: typed}
	case []byte:
		if utf8Safe(typed) {
			return value{T: "str", S: string(typed)}
		}
		return value{T: "bytes", V: typed}
	case int64:
		return value{T: "int", I: typed}
	case float64:
		return value{T: "float", F: typed}
	case bool:
		return value{T: "bool", B: typed}
	case time.Time:
		return value{T: "time", S: typed.UTC().Format(time.RFC3339Nano)}
	default:
		return value{T: "str", S: fmt.Sprint(typed)}
	}
}

func utf8Safe(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

func (v value) decode() (any, error) {
	switch v.T {
	case "null":
		return nil, nil
	case "str":
		return v.S, nil
	case "bytes":
		return v.V, nil
	case "int":
		return v.I, nil
	case "float":
		return v.F, nil
	case "bool":
		return v.B, nil
	case "time":
		parsed, err := time.Parse(time.RFC3339Nano, v.S)
		if err != nil {
			return nil, fmt.Errorf("parse backed-up timestamp %q: %w", v.S, err)
		}
		return parsed, nil
	}
	return nil, fmt.Errorf("unknown backup value type %q", v.T)
}

// readBackup streams a backup file and returns the header, the per-table row
// values keyed by primary key, and the recorded counts.
func readBackup(path string) (backupRecord, map[string][]map[string]any, map[string]int, error) {
	file, err := os.Open(path)
	if err != nil {
		return backupRecord{}, nil, nil, fmt.Errorf("open backup: %w", err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return backupRecord{}, nil, nil, fmt.Errorf("read backup gzip: %w", err)
	}
	defer gz.Close()

	decoder := json.NewDecoder(bufio.NewReader(gz))
	var header backupRecord
	columns := map[string][]string{}
	rowsByTable := map[string][]map[string]any{}
	counts := map[string]int{}

	for {
		var rec backupRecord
		if err := decoder.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return backupRecord{}, nil, nil, fmt.Errorf("parse backup: %w", err)
		}
		switch rec.Kind {
		case "header":
			header = rec
		case "table":
			columns[rec.Table] = rec.Columns
		case "row":
			cols, ok := columns[rec.Table]
			if !ok {
				return backupRecord{}, nil, nil, fmt.Errorf("backup row for %s appears before its column list", rec.Table)
			}
			if len(cols) != len(rec.Values) {
				return backupRecord{}, nil, nil, fmt.Errorf("backup row for %s has %d values for %d columns", rec.Table, len(rec.Values), len(cols))
			}
			rowMap := make(map[string]any, len(cols))
			for i, col := range cols {
				decoded, err := rec.Values[i].decode()
				if err != nil {
					return backupRecord{}, nil, nil, err
				}
				rowMap[col] = decoded
			}
			rowsByTable[rec.Table] = append(rowsByTable[rec.Table], rowMap)
		case "footer":
			counts = rec.Counts
		}
	}
	if header.Kind != "header" {
		return backupRecord{}, nil, nil, errors.New("backup is missing its header record")
	}
	if header.Tool != toolName || header.Version != backupVer {
		return backupRecord{}, nil, nil, fmt.Errorf("backup was written by an unsupported tool/version: %s v%d", header.Tool, header.Version)
	}
	return header, rowsByTable, counts, nil
}

func (d *database) verifyBackup(path string) (map[string]int, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("a backup is required before changing secrets; pass -backup=<file> or -backup-dir=<dir>")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("backup file %s is not readable: %w", path, err)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("backup file %s is empty", path)
	}
	header, _, recorded, err := readBackup(path)
	if err != nil {
		return nil, err
	}
	if header.DSNSHA256 != d.dsnHash {
		return nil, fmt.Errorf("backup %s was taken from a different database (dsn sha256 mismatch)", path)
	}
	current := map[string]int{}
	for _, table := range []string{channelsTbl, optionsTbl} {
		var n int
		if err := d.db.QueryRow("SELECT COUNT(*) FROM " + d.table(table)).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		current[table] = n
		if recorded[table] != n {
			return nil, fmt.Errorf("backup %s is stale: %s had %d rows when it was taken, %d now (take a fresh backup)", path, table, recorded[table], n)
		}
	}
	return current, nil
}

// ---------------------------------------------------------------------------
// encrypt / decrypt
// ---------------------------------------------------------------------------

func (d *database) transform(mode, backupFile, backupDir string, dryRun bool) error {
	if mode == "encrypt" && !common.SecretEncryptionEnabled() {
		return fmt.Errorf("encryption is not enabled: set %s or %s%s first", common.SecretEncryptionKeyEnv, common.SecretEncryptionKeyEnv, common.SecretFileSuffix)
	}
	if mode == "decrypt" && !common.SecretEncryptionEnabled() {
		return fmt.Errorf("rollback needs the same master key that encrypted the rows: set %s or %s%s", common.SecretEncryptionKeyEnv, common.SecretEncryptionKeyEnv, common.SecretFileSuffix)
	}

	backupPath := backupFile
	if strings.TrimSpace(backupPath) == "" {
		if strings.TrimSpace(backupDir) == "" {
			return errors.New("refusing to change secrets without a backup: pass -backup=<file> or -backup-dir=<dir>")
		}
		path, counts, err := d.writeBackup(backupDir)
		if err != nil {
			return err
		}
		fmt.Printf("backup written: %s\nrow counts: %v\n", path, counts)
		backupPath = path
	}
	if _, err := d.verifyBackup(backupPath); err != nil {
		return err
	}
	fmt.Printf("backup verified: %s\n", backupPath)

	if dryRun {
		fmt.Println("dry-run: no rows will be written")
	}

	channelStats, err := d.transformTable(mode, channelsTbl, "id", "key", "", dryRun)
	if err != nil {
		return err
	}
	optionStats, err := d.transformTable(mode, optionsTbl, "key", "value", optionsTbl, dryRun)
	if err != nil {
		return err
	}

	verb := "encrypted"
	if mode == "decrypt" {
		verb = "decrypted"
	}
	fmt.Printf("channels.key: %s=%d already=%d empty=%d\n", verb, channelStats.Changed, channelStats.Encrypted, channelStats.Empty)
	fmt.Printf("options.value: %s=%d already=%d empty=%d\n", verb, optionStats.Changed, optionStats.Encrypted, optionStats.Empty)
	if dryRun {
		fmt.Println("nothing was written (dry-run)")
	} else {
		fmt.Println("restart new-api so every instance reloads channels and options from the database")
	}
	return nil
}

// transformTable rewrites one column. In encrypt mode "already" counts rows
// that were already envelopes; in decrypt mode it counts rows that were
// already plaintext (because the rollback stopped halfway or was never run).
func (d *database) transformTable(mode, table, idCol, secretCol, optionFilter string, dryRun bool) (stats, error) {
	var out stats
	query := fmt.Sprintf("SELECT %s, %s FROM %s", d.quote(idCol), d.quote(secretCol), d.table(table))
	rows, err := d.db.Query(query)
	if err != nil {
		return out, fmt.Errorf("read %s: %w", table, err)
	}
	defer rows.Close()

	type pending struct {
		id      any
		current string
		next    string
	}
	var work []pending
	for rows.Next() {
		var idRaw any
		var secret sql.NullString
		if err := rows.Scan(&idRaw, &secret); err != nil {
			return out, err
		}
		idText := ""
		if idRaw != nil {
			idText = fmt.Sprint(normalizeID(idRaw))
		}
		if optionFilter != "" && !model.IsSensitiveOption(idText) {
			continue
		}
		out.Scanned++
		if !secret.Valid || secret.String == "" {
			out.Empty++
			continue
		}
		encrypted := common.IsEncryptedSecret(secret.String)
		if mode == "encrypt" {
			if encrypted {
				out.Encrypted++
				continue
			}
			next, err := common.EncryptSecret(secret.String)
			if err != nil {
				return out, fmt.Errorf("encrypt %s %s: %w", table, idText, err)
			}
			work = append(work, pending{id: normalizeID(idRaw), current: secret.String, next: next})
		} else {
			if !encrypted {
				out.Encrypted++
				continue
			}
			next, err := common.DecryptSecret(secret.String)
			if err != nil {
				return out, fmt.Errorf("decrypt %s %s: %w (rollback aborted, nothing written)", table, idText, err)
			}
			work = append(work, pending{id: normalizeID(idRaw), current: secret.String, next: next})
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()

	if dryRun || len(work) == 0 {
		out.Changed = len(work)
		return out, nil
	}

	tx, err := d.db.Begin()
	if err != nil {
		return out, err
	}
	stmt, err := tx.Prepare(fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ? AND %s = ?",
		d.table(table), d.quote(secretCol), d.quote(idCol), d.quote(secretCol)))
	if err != nil {
		tx.Rollback()
		return out, err
	}
	defer stmt.Close()
	for _, item := range work {
		result, err := stmt.Exec(item.next, item.id, item.current)
		if err != nil {
			tx.Rollback()
			return out, fmt.Errorf("update %s: %w", table, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			tx.Rollback()
			return out, err
		}
		if affected != 1 {
			tx.Rollback()
			return out, fmt.Errorf("update %s: row %v changed underneath the migration (expected 1 row, got %d); nothing was written", table, item.id, affected)
		}
		out.Changed++
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

func normalizeID(id any) any {
	if b, ok := id.([]byte); ok {
		return string(b)
	}
	return id
}

// ---------------------------------------------------------------------------
// restore
// ---------------------------------------------------------------------------

func (d *database) restore(path string, dryRun bool) error {
	header, rowsByTable, counts, err := readBackup(path)
	if err != nil {
		return err
	}
	if header.DSNSHA256 != d.dsnHash {
		return fmt.Errorf("backup %s was taken from a different database (dsn sha256 mismatch)", path)
	}
	fmt.Printf("restoring from %s (taken %s), recorded counts: %v\n", path, header.CreatedAt, counts)

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, table := range []string{channelsTbl, optionsTbl} {
		rows := rowsByTable[table]
		idCol := "id"
		if table == optionsTbl {
			idCol = "key"
		}
		written := 0
		for _, rowValues := range rows {
			columns := make([]string, 0, len(rowValues))
			for col := range rowValues {
				columns = append(columns, col)
			}
			sort.Strings(columns)
			idValue, ok := rowValues[idCol]
			if !ok {
				return fmt.Errorf("backup row for %s has no %s column", table, idCol)
			}

			var existing int
			if err := tx.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = ?", d.table(table), d.quote(idCol)), idValue).Scan(&existing); err != nil {
				return fmt.Errorf("check %s: %w", table, err)
			}
			if dryRun {
				written++
				continue
			}
			if existing > 0 {
				setParts := make([]string, 0, len(columns))
				args := make([]any, 0, len(columns))
				for _, col := range columns {
					if col == idCol {
						continue
					}
					setParts = append(setParts, d.quote(col)+" = ?")
					args = append(args, rowValues[col])
				}
				args = append(args, idValue)
				if _, err := tx.Exec(fmt.Sprintf("UPDATE %s SET %s WHERE %s = ?", d.table(table), strings.Join(setParts, ", "), d.quote(idCol)), args...); err != nil {
					return fmt.Errorf("restore update %s: %w", table, err)
				}
			} else {
				placeholders := make([]string, len(columns))
				args := make([]any, len(columns))
				for i, col := range columns {
					placeholders[i] = "?"
					args[i] = rowValues[col]
				}
				if _, err := tx.Exec(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", d.table(table), strings.Join(quoted(d.quote, columns), ", "), strings.Join(placeholders, ", ")), args...); err != nil {
					return fmt.Errorf("restore insert %s: %w", table, err)
				}
			}
			written++
		}
		fmt.Printf("%s: %d rows %s\n", table, written, map[bool]string{true: "would be restored", false: "restored"}[dryRun])
	}
	if dryRun {
		fmt.Println("nothing was written (dry-run)")
		return nil
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Println("restored; restart new-api afterwards")
	return nil
}

func quoted(fn func(string) string, idents []string) []string {
	out := make([]string, len(idents))
	for i, ident := range idents {
		out[i] = fn(ident)
	}
	return out
}
