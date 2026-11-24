package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This is an intentionally conservative source-level inventory, not a proof
// of alias freedom. Count reads as well as writes: an apparently read-only
// selector can leak a mutable pointer or be passed to an unknown callback.
// New access sites require review before any generation-cache fast path.
func TestStorageMetadataDirectAccessInventory(t *testing.T) {
	expected := []string{
		"archive_update_transaction.go:archiveUpdatePrepare",
		"archive_update_transaction.go:publishArchiveUpdate",
		"import_page_batch.go:commitImportPage",
		"library_management.go:changeShare",
		"library_management.go:claimQueued",
		"library_management.go:createUniqueJob",
		"library_management.go:purgeOwnedBeforeUnderGate",
		"library_management.go:removeOwned",
		"library_management.go:restoreOwned",
		"library_management.go:setEveryone",
		"library_management.go:transitionToQueued",
		"library_management.go:trashed",
		"library_management.go:trashedJob",
		"metadata_transaction.go:saveJobsImportsLocked",
		"revisions.go:restoreArchiveRevision",
		"revisions.go:saveArchiveRevisionLocked",
		"sqlite_history_delete.go:deleteSQLHistoryLocked",
		"sqlite_primary_read_adapter.go:guardArchiveFileMutation",
		"sqlite_primary_read_adapter.go:sqlitePrimaryGuardMutationLocked",
		"sqlite_primary_read_adapter.go:sqlitePrimaryReadSnapshotLocked",
		"sqlite_primary_startup.go:openSQLPrimaryStorageLocked",
		"sqlite_primary_storage_adapter.go:saveSQLitePrimaryLocked",
		"storage.go:all",
		"storage.go:get",
		"storage.go:migrateArchiveTimestamps",
		"storage.go:migrateLegacyJobs",
		"storage.go:newStorageMode",
		"storage.go:queued",
		"storage.go:saveJobsLocked",
		"storage_metadata_replace.go:replaceStorageBatchLocked",
		"storage_metadata_replace.go:replaceStorageJobLocked",
		"subscription_imports.go:cancelImport",
		"subscription_imports.go:claimImport",
		"subscription_imports.go:createImports",
		"subscription_imports.go:importCanceled",
		"subscription_imports.go:importsFor",
		"subscription_imports.go:loadImports",
		"subscription_imports.go:queuedImports",
		"subscription_imports.go:saveImportsLocked",
		"subscription_imports.go:scheduleDueImportsAllowed",
		"subscription_imports.go:setImportSchedule",
		"subscription_imports.go:updateImport",
		"trash_cleanup.go:expiredTrash",
		"video_publication.go:publishVideoRendition",
	}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				// Reject global initializers that touch these field names.
				if metadataDirectAccess(decl) {
					t.Errorf("metadata selector outside function: %s", path)
				}
				continue
			}
			if metadataDirectAccess(fn.Body) {
				actual = append(actual, filepath.Base(path)+":"+fn.Name.Name)
			}
		}
	}
	slices.Sort(actual)
	slices.Sort(expected)
	for _, name := range actual {
		if !slices.Contains(expected, name) {
			t.Errorf("unreviewed metadata access: %s", name)
		}
	}
	for _, name := range expected {
		if !slices.Contains(actual, name) {
			t.Errorf("stale metadata access inventory: %s", name)
		}
	}
}

func metadataDirectAccess(node ast.Node) bool {
	if node == nil {
		return false
	}
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		selector, ok := n.(*ast.SelectorExpr)
		if ok && (selector.Sel.Name == "jobs" || selector.Sel.Name == "imports") {
			found = true
		}
		return !found
	})
	return found
}

func TestMetadataDirectAccessClassifier(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"s.jobs[id]", true},
		{"a.store.imports[id].State", true},
		{"func() { delete(s.jobs, id) }", true},
		{"cloneStorageJob(job)", false},
		{"\"s.jobs\"", false},
	} {
		node, err := parser.ParseExpr(tc.source)
		if err != nil {
			t.Fatal(err)
		}
		if got := metadataDirectAccess(node); got != tc.want {
			t.Errorf("%s: access=%v want=%v", tc.source, got, tc.want)
		}
	}
}
