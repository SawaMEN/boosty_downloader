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
	flag.Parse()
	if *root == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: boosty-dbcheck --data-dir /var/lib/boosty-downloader")
		os.Exit(2)
	}
	if err := server.VerifySQLiteExport(*root); err != nil {
		fmt.Fprintln(os.Stderr, "SQLite preflight failed:", err)
		os.Exit(1)
	}
	fmt.Println("SQLite preflight OK: JSON jobs, author imports, owners and grants match the read-only export")
}
