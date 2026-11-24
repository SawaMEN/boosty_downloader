package server

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// Compare planning an unchanged shadow inventory, using the same synthetic
// schema and desired records for both paths. Neither path mutates the DB.
func BenchmarkSQLiteShadowUnchangedPlan(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("archives-%d", n), func(b *testing.B) {
			store := sqlScaleFixture(b, n)
			jobs := make(map[string]Job, n)
			for id, job := range store.jobs {
				jobs[id] = *job
			}
			imports := map[string]AuthorImport{}
			ctx := context.Background()
			for _, mode := range []string{"materialized", "streaming"} {
				b.Run(mode, func(b *testing.B) {
					run := func() {
						tx, err := store.primary.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
						if err != nil {
							b.Fatal(err)
						}
						defer tx.Rollback()
						if mode == "streaming" {
							plan, err := sqlitePlanShadowDelta(ctx, tx, jobs, imports)
							if err != nil {
								b.Fatal(err)
							}
							if len(plan.archives)+len(plan.removeArchives)+len(plan.importChanges)+len(plan.removeImports) != 0 {
								b.Fatal("unchanged fixture generated changes")
							}
						} else {
							stored, err := sqliteShadowSQLJobs(ctx, tx)
							if err != nil {
								b.Fatal(err)
							}
							storedImports, err := sqliteShadowSQLImports(ctx, tx)
							if err != nil {
								b.Fatal(err)
							}
							if len(stored) != len(jobs) || len(storedImports) != len(imports) {
								b.Fatal("fixture inventory differs")
							}
							for id, want := range jobs {
								if have, ok := stored[id]; !ok || !sqliteShadowEqual(have, want) {
									b.Fatal("fixture metadata differs")
								}
							}
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						run()
					}
				})
			}
		})
	}
}
