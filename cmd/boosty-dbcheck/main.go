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
	flag.Parse()
	if *root == "" || flag.NArg() != 0 || (*backup != "" && *auditMedia) {
		fmt.Fprintln(os.Stderr, "usage: boosty-dbcheck --data-dir /var/lib/boosty-downloader [--backup-dir /secure/backups/boosty-YYYYMMDD | --audit-media]")
		os.Exit(2)
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
