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
	auditMedia := flag.Bool("audit-media", false, "read-only audit of SQL snapshot index against revision directories and SHA-256 media bytes")
	verifyBackup := flag.String("verify-backup-dir", "", "read-only verify a metadata recovery backup and its JSON/SQL parity")
	flag.Parse()
	if flag.NArg() != 0 || (*verifyBackup != "" && (*root != "" || *backup != "" || *auditMedia)) ||
		(*verifyBackup == "" && (*root == "" || (*backup != "" && *auditMedia))) {
		fmt.Fprintln(os.Stderr, "usage: boosty-dbcheck --data-dir DATA [--backup-dir NEW-DIR | --audit-media] | --verify-backup-dir EXISTING-DIR")
		os.Exit(2)
	}
	if *verifyBackup != "" {
		if err := server.VerifySQLiteMetadataBackup(*verifyBackup); err != nil {
			fmt.Fprintln(os.Stderr, "Metadata recovery backup verification failed:", err)
			os.Exit(1)
		}
		fmt.Println("Metadata recovery backup OK: SHA-256 manifest, JSON, SQLite schema and owner ACLs match")
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
