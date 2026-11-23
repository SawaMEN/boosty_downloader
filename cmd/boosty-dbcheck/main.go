// Command boosty-dbcheck validates an offline SQLite export against JSON
// without starting the web server or any archive workers.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/SawaMEN/boosty_downloader/internal/server"
)

func main() {
	root := flag.String("data-dir", "", "the stopped server's data directory")
	backup := flag.String("backup-dir", "", "create a new private metadata rollback backup directory (must not exist)")
	fullSnapshot := flag.String("full-snapshot-dir", "", "create a new private, SHA-256 verified copy of the whole stopped data directory, including media")
	verifyFullSnapshot := flag.String("verify-full-snapshot-dir", "", "independently verify a full data directory snapshot with no live server access")
	restoreFullSnapshot := flag.String("restore-full-snapshot-dir", "", "verified full snapshot to recover into --restore-to (new, nonexistent directory only)")
	restoreTo := flag.String("restore-to", "", "new detached destination for offline restore; no existing installation may be replaced")
	restoreSQL := flag.String("restore-sqlite-json-dir", "", "verified newer SQL-to-JSON metadata export for a detached combined restore")
	fromFull := flag.String("from-full-snapshot-dir", "", "verified full data snapshot to combine with --restore-sqlite-json-dir")
	auditMedia := flag.Bool("audit-media", false, "read-only audit of SQL snapshot index against revision directories and SHA-256 media bytes")
	verifyBackup := flag.String("verify-backup-dir", "", "read-only verify a metadata recovery backup and its JSON/SQL parity")
	reindexMedia := flag.Bool("reindex-media", false, "offline transactional rebuild of the SQLite media/revision index")
	restoreShadow := flag.Bool("restore-sqlite-shadow", false, "offline atomic restoration of only the detached SQLite index from a verified backup; JSON is never overwritten")
	recoveryDir := flag.String("recovery-dir", "", "required VERIFIED metadata backup used by --reindex-media or --cutover-preflight")
	cutoverPreflight := flag.Bool("cutover-preflight", false, "read-only preflight of JSON ACLs, SQL media index and an identical recovery backup")
	cutoverFull := flag.String("cutover-full-snapshot-dir", "", "require a verified exact full data snapshot with --cutover-preflight or --prepare-sqlite-cutover")
	activatePrimary := flag.Bool("activate-sqlite-primary", false, "EXPERIMENTAL offline activation; requires verified metadata and full data snapshots")
	confirmPrimary := flag.Bool("confirm-experimental-sqlite", false, "explicitly accept experimental SQL-primary activation")
	prepareCutover := flag.Bool("prepare-sqlite-cutover", false, "durably stage a VERIFIED offline SQL cutover rehearsal without activating SQL-primary")
	recoverCutover := flag.Bool("recover-sqlite-cutover", false, "safely cancel a prepared offline cutover intent after validating the pinned recovery point")
	exportSQL := flag.String("export-sqlite-json-dir", "", "create a NEW detached verified SQL-to-JSON recovery metadata snapshot; does not change live JSON")
	rollbackSQL := flag.Bool("rollback-sqlite-to-json", false, "offline rollback of explicitly selected SQLite using its newest verified jobs and imports")
	rollbackFull := flag.String("rollback-full-snapshot-dir", "", "required exact full data snapshot for --rollback-sqlite-to-json")
	recoverRollback := flag.Bool("recover-sqlite-rollback", false, "resume an interrupted verified SQLite-to-JSON rollback")
	pinJSON := flag.Bool("pin-json-backend", false, "persist a private backend=json selector on a stopped server; never activates SQLite-primary")
	flag.Parse()
	// Exactly one operation per invocation, with an explicit verified backup
	// for any operation that claims a fixed recovery point.
	modes := 0
	for _, enabled := range []bool{*backup != "", *fullSnapshot != "", *auditMedia, *reindexMedia, *cutoverPreflight, *restoreShadow, *prepareCutover, *recoverCutover, *exportSQL != "", *pinJSON, *rollbackSQL, *recoverRollback, *activatePrimary} {
		if enabled {
			modes++
		}
	}
	needsRecovery := *reindexMedia || *cutoverPreflight || *restoreShadow || *prepareCutover || *activatePrimary
	verifiers := 0
	if *verifyBackup != "" {
		verifiers++
	}
	if *verifyFullSnapshot != "" {
		verifiers++
	}
	if *restoreFullSnapshot != "" {
		verifiers++
	}
	if *restoreSQL != "" {
		verifiers++
	}
	if flag.NArg() != 0 || verifiers > 1 ||
		(*rollbackSQL != (*rollbackFull != "")) ||
		(*activatePrimary != *confirmPrimary) ||
		(*activatePrimary && *cutoverFull == "") ||
		(*cutoverFull != "" && !*cutoverPreflight && !*prepareCutover && !*activatePrimary) ||
		(*restoreTo != "" && *restoreFullSnapshot == "" && *restoreSQL == "") ||
		(*restoreFullSnapshot != "" && *restoreTo == "") ||
		(*restoreSQL != "" && (*fromFull == "" || *restoreTo == "")) ||
		(*fromFull != "" && *restoreSQL == "") ||
		(verifiers != 0 && (*root != "" || modes != 0 || *recoveryDir != "")) ||
		(verifiers == 0 && (*root == "" || modes > 1 || (needsRecovery != (*recoveryDir != "")))) {
		fmt.Fprintln(os.Stderr, "usage: boosty-dbcheck --data-dir DATA [--backup-dir NEW-DIR | --full-snapshot-dir NEW-DIR | --audit-media | --reindex-media --recovery-dir VERIFIED-BACKUP | --cutover-preflight --recovery-dir VERIFIED-BACKUP [--cutover-full-snapshot-dir VERIFIED-FULL-SNAPSHOT] | --restore-sqlite-shadow --recovery-dir VERIFIED-BACKUP | --prepare-sqlite-cutover --recovery-dir VERIFIED-BACKUP [--cutover-full-snapshot-dir VERIFIED-FULL-SNAPSHOT] | --activate-sqlite-primary --confirm-experimental-sqlite --recovery-dir VERIFIED-BACKUP --cutover-full-snapshot-dir VERIFIED-FULL-SNAPSHOT | --recover-sqlite-cutover | --export-sqlite-json-dir NEW-DIR | --pin-json-backend | --rollback-sqlite-to-json --rollback-full-snapshot-dir VERIFIED-FULL-SNAPSHOT | --recover-sqlite-rollback] | --verify-backup-dir EXISTING-DIR | --verify-full-snapshot-dir EXISTING-DIR | --restore-full-snapshot-dir VERIFIED-SNAPSHOT --restore-to NEW-DIR | --restore-sqlite-json-dir VERIFIED-SQL-EXPORT --from-full-snapshot-dir VERIFIED-FULL-SNAPSHOT --restore-to NEW-DIR")
		os.Exit(2)
	}
	if *restoreSQL != "" {
		if err := server.RestoreDetachedSQLRecovery(*fromFull, *restoreSQL, *restoreTo); err != nil {
			fmt.Fprintln(os.Stderr, "Detached combined SQL/JSON full recovery rejected:", err)
			os.Exit(1)
		}
		fmt.Println("SQL/JSON metadata and full data restored to a NEW DETACHED directory:", *restoreTo)
		fmt.Println("Original SQL export, full snapshot and live server were not changed.")
		fmt.Println("This is NOT an in-place SQL-primary rollback; verify host configuration, ownership and TLS before any test service startup.")
		return
	}
	if *restoreFullSnapshot != "" {
		if err := server.RestoreFullDataSnapshot(*restoreFullSnapshot, *restoreTo); err != nil {
			fmt.Fprintln(os.Stderr, "Detached full data restore rejected:", err)
			os.Exit(1)
		}
		fmt.Println("Full data snapshot restored to a NEW DETACHED directory:", *restoreTo)
		fmt.Println("Source installation was not modified. Files remain private (0600), directories (0700).")
		fmt.Println("Host /etc config, external TLS keys, ownership and binaries are NOT restored; do NOT point a live service here without a separate review.")
		return
	}
	if *verifyFullSnapshot != "" {
		if err := server.VerifyFullDataSnapshot(*verifyFullSnapshot); err != nil {
			fmt.Fprintln(os.Stderr, "Full data snapshot verification failed:", err)
			os.Exit(1)
		}
		fmt.Println("Full data snapshot OK: private file inventory and all SHA-256 hashes match.")
		fmt.Println("Host /etc config, external TLS keys and executable binaries are NOT included.")
		return
	}
	if *verifyBackup != "" {
		if err := server.VerifySQLiteMetadataBackup(*verifyBackup); err != nil {
			fmt.Fprintln(os.Stderr, "Metadata recovery backup verification failed:", err)
			os.Exit(1)
		}
		fmt.Println("Metadata recovery backup OK: SHA-256 manifest, JSON, SQLite schema and owner ACLs match")
		return
	}
	if *rollbackSQL {
		if err := server.RollbackSQLiteToJSON(*root, *rollbackFull); err != nil {
			fmt.Fprintln(os.Stderr, "Offline SQLite rollback rejected or interrupted:", err)
			os.Exit(1)
		}
		fmt.Println("Verified latest SQL jobs/imports published to JSON; JSON backend selected. SQLite preserved under its retired SHA-256 filename; media unchanged.")
		return
	}
	if *recoverRollback {
		if err := server.RecoverSQLiteRollback(*root); err != nil {
			fmt.Fprintln(os.Stderr, "SQLite rollback recovery rejected:", err)
			os.Exit(1)
		}
		fmt.Println("Pending SQLite-to-JSON rollback completed or no intent exists.")
		return
	}
	if *pinJSON {
		if err := server.PinJSONBackendSelection(*root); err != nil {
			fmt.Fprintln(os.Stderr, "Offline JSON backend selection rejected:", err)
			os.Exit(1)
		}
		fmt.Println("Verified backend selector is pinned to JSON-primary. SQLite-primary has NOT been activated.")
		fmt.Println("WARNING: future JSON writes without a synchronized SQLite index may BLOCK the next startup; not a production cutover.")
		return
	}
	if *exportSQL != "" {
		if err := server.ExportSQLiteMetadataToJSON(*root, *exportSQL); err != nil {
			fmt.Fprintln(os.Stderr, "Detached SQL-to-JSON export rejected:", err)
			os.Exit(1)
		}
		fmt.Println("Private detached SQL/JSON recovery metadata created and verified:", *exportSQL)
		fmt.Println("Live JSON/SQL, media and backend were NOT modified. This does not activate or roll back SQL-primary.")
		return
	}
	if *activatePrimary {
		if err := server.ActivateSQLitePrimaryExperimental(*root, *recoveryDir, *cutoverFull); err != nil {
			fmt.Fprintln(os.Stderr, "Experimental SQLite activation rejected:", err)
			os.Exit(1)
		}
		fmt.Println("EXPERIMENTAL SQLite-primary selected. Start only with BD_SQLITE_PRIMARY=1 and all shadow/verify modes disabled.")
		fmt.Println("Keep both backups. To return to JSON, stop the server, create a fresh full snapshot and run --rollback-sqlite-to-json; then unset BD_SQLITE_PRIMARY.")
		return
	}
	if *prepareCutover {
		var err error
		if *cutoverFull != "" {
			err = server.PrepareSQLiteCutoverFullRehearsal(*root, *recoveryDir, *cutoverFull)
		} else {
			err = server.PrepareSQLiteCutoverRehearsal(*root, *recoveryDir)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "SQLite cutover rehearsal failed:", err)
			os.Exit(1)
		}
		fmt.Println("Prepared SQLite cutover intent is durable. SQL-primary remains DISABLED; JSON and media were not modified.")
		fmt.Println("On the next server startup, a verified unchanged recovery point safely cancels this unfinished intent.")
		return
	}
	if *recoverCutover {
		if err := server.RecoverSQLiteCutoverRehearsal(*root); err != nil {
			fmt.Fprintln(os.Stderr, "SQLite cutover cancellation rejected:", err)
			os.Exit(1)
		}
		fmt.Println("SQLite cutover rehearsal canceled or already absent. JSON remains authoritative.")
		return
	}
	if *cutoverPreflight {
		var err error
		if *cutoverFull != "" {
			err = server.VerifySQLiteCutoverFullPreflight(*root, *recoveryDir, *cutoverFull)
		} else {
			err = server.VerifySQLiteCutoverPreflight(*root, *recoveryDir)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "SQLite cutover preflight failed:", err)
			os.Exit(1)
		}
		fmt.Println("SQLite preflight OK: JSON ACLs, SQL revision/media index and private recovery metadata are consistent.")
		fmt.Println("This is NOT a production backend switch; a complete filesystem/VPS backup and tested rollback remain mandatory.")
		return
	}
	if *restoreShadow {
		if err := server.RestoreSQLiteShadowFromBackup(*root, *recoveryDir); err != nil {
			fmt.Fprintln(os.Stderr, "SQLite shadow restore rejected:", err)
			os.Exit(1)
		}
		fmt.Println("Detached SQLite shadow restored and verified. JSON remains authoritative and was not modified.")
		fmt.Println("This is NOT a full filesystem/media/account/token restore or a production backend switch.")
		return
	}
	if *reindexMedia {
		if err := server.ReindexSQLiteRevisionInventory(*root, *recoveryDir); err != nil {
			fmt.Fprintln(os.Stderr, "Offline SQLite revision reindex failed:", err)
			os.Exit(1)
		}
		fmt.Println("SQLite revision/media index updated transactionally and verified; JSON remains authoritative.")
		return
	}
	if *fullSnapshot != "" {
		if err := server.PrepareFullDataSnapshot(*root, *fullSnapshot); err != nil {
			fmt.Fprintln(os.Stderr, "Full data snapshot failed:", err)
			os.Exit(1)
		}
		fmt.Println("Full application DATA DIRECTORY snapshot created and independently verified:", *fullSnapshot)
		fmt.Println("Host /etc config, external TLS keys and binaries are NOT included. This does NOT activate SQL-primary or restore data.")
		return
	}
	if *backup != "" {
		if err := server.PrepareSQLiteBackup(*root, *backup); err != nil {
			fmt.Fprintln(os.Stderr, "SQLite cutover backup failed:", err)
			os.Exit(1)
		}
		fmt.Println("Metadata recovery snapshot created:", *backup)
		fmt.Println("This is NOT a media backup and does NOT activate the SQLite backend.")
		return
	}
	if *auditMedia {
		if err := server.VerifySQLiteSnapshotInventory(*root); err != nil {
			fmt.Fprintln(os.Stderr, "SQLite archive/media inventory failed:", err)
			os.Exit(1)
		}
		fmt.Println("SQLite snapshot inventory OK: archive revisions and media SHA-256 match disk")
		fmt.Println("This is a read-only audit; no files or SQL rows were modified.")
		return
	}
	if err := server.VerifySQLiteExport(*root); err != nil {
		fmt.Fprintln(os.Stderr, "SQLite preflight failed:", err)
		os.Exit(1)
	}
	fmt.Println("SQLite preflight OK: JSON jobs, author imports, owners and grants match the read-only export")
}
