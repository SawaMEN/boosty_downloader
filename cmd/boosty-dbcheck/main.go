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
	auditMedia := flag.Bool("audit-media", false, "read-only audit of SQL snapshot index against revision directories and SHA-256 media bytes")
	verifyBackup := flag.String("verify-backup-dir", "", "read-only verify a metadata recovery backup and its JSON/SQL parity")
	reindexMedia := flag.Bool("reindex-media", false, "offline transactional rebuild of the SQLite media/revision index")
	restoreShadow := flag.Bool("restore-sqlite-shadow", false, "offline atomic restoration of only the detached SQLite index from a verified backup; JSON is never overwritten")
	recoveryDir := flag.String("recovery-dir", "", "required VERIFIED metadata backup used by --reindex-media or --cutover-preflight")
	cutoverPreflight := flag.Bool("cutover-preflight", false, "read-only preflight of JSON ACLs, SQL media index and an identical recovery backup")
	flag.Parse()
	// Exactly one operation per invocation, with an explicit verified backup
	// for any operation that claims a fixed recovery point.
	modes := 0
	for _, enabled := range []bool{*backup != "", *fullSnapshot != "", *auditMedia, *reindexMedia, *cutoverPreflight, *restoreShadow} {
		if enabled {
			modes++
		}
	}
	needsRecovery := *reindexMedia || *cutoverPreflight || *restoreShadow
	verifiers := 0
	if *verifyBackup != "" {
		verifiers++
	}
	if *verifyFullSnapshot != "" {
		verifiers++
	}
	if flag.NArg() != 0 || verifiers > 1 ||
		(verifiers != 0 && (*root != "" || modes != 0 || *recoveryDir != "")) ||
		(verifiers == 0 && (*root == "" || modes > 1 || (needsRecovery != (*recoveryDir != "")))) {
		fmt.Fprintln(os.Stderr, "usage: boosty-dbcheck --data-dir DATA [--backup-dir NEW-DIR | --full-snapshot-dir NEW-DIR | --audit-media | --reindex-media --recovery-dir VERIFIED-BACKUP | --cutover-preflight --recovery-dir VERIFIED-BACKUP | --restore-sqlite-shadow --recovery-dir VERIFIED-BACKUP] | --verify-backup-dir EXISTING-DIR | --verify-full-snapshot-dir EXISTING-DIR")
		os.Exit(2)
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
	if *cutoverPreflight {
		if err := server.VerifySQLiteCutoverPreflight(*root, *recoveryDir); err != nil {
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
