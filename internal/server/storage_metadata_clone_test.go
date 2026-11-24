package server

import (
	"reflect"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func TestStorageJobOwnershipBoundaries(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{
		ID: "job", OwnerID: "owner", Author: "author", PostID: "post",
		State: "queued", CreatedAt: time.Now().UTC(), SharedWith: []string{"member"},
		Subscription: &CapturedSubscription{Level: "original"},
		RequiredTier: &boosty.PostRequirement{TierName: "original"},
	}
	mutate := func(j Job) {
		j.SharedWith[0] = "forged"
		j.Subscription.Level = "forged"
		j.RequiredTier.TierName = "forged"
	}
	check := func() {
		t.Helper()
		j, ok := s.get(job.ID)
		if !ok || j.SharedWith[0] != "member" || j.Subscription.Level != "original" || j.RequiredTier.TierName != "original" {
			t.Fatalf("storage-owned metadata changed: %+v", j)
		}
	}
	created, ok, err := s.createUniqueJob(job)
	if err != nil || !ok {
		t.Fatalf("create: %v", err)
	}
	mutate(job)
	mutate(created)
	check()
	got, _ := s.get(job.ID)
	mutate(got)
	mutate(s.all()[0])
	check()
	duplicate, createdAgain, err := s.createUniqueJob(job)
	if err != nil || createdAgain {
		t.Fatalf("duplicate: %v", err)
	}
	mutate(duplicate)
	check()
	claimed, ok := s.claimQueued(job.ID)
	if !ok {
		t.Fatal("claim failed")
	}
	mutate(claimed)
	check()
	update, _ := s.get(job.ID)
	update.Title = "updated"
	if err := s.upsert(update); err != nil {
		t.Fatal(err)
	}
	mutate(update)
	check()
}

func TestStorageImportOwnershipBoundaries(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	item := AuthorImport{ID: "import", OwnerID: "owner", Author: "author", State: "queued",
		Subscription: &CapturedSubscription{Level: "original"}}
	added, err := s.createImports([]AuthorImport{item})
	if err != nil || len(added) != 1 {
		t.Fatalf("create import: %v", err)
	}
	item.Subscription.Level = "forged"
	added[0].Subscription.Level = "forged"
	check := func() {
		t.Helper()
		items := s.importsFor("owner")
		if len(items) != 1 || items[0].Subscription.Level != "original" {
			t.Fatalf("stored import changed: %+v", items)
		}
		items[0].Subscription.Level = "forged"
	}
	check()
	claimed, ok := s.claimImport(item.ID)
	if !ok {
		t.Fatal("claim import failed")
	}
	claimed.Subscription.Level = "forged"
	check()
	update := s.importsFor("owner")[0]
	if err := s.updateImport(update); err != nil {
		t.Fatal(err)
	}
	update.Subscription.Level = "forged"
	check()
}

func TestStorageCloneDeletedTimestamp(t *testing.T) {
	stamp := time.Now().UTC()
	original := Job{DeletedAt: &stamp}
	copied := cloneStorageJob(original)
	*copied.DeletedAt = stamp.Add(time.Hour)
	if !original.DeletedAt.Equal(stamp) || original.DeletedAt == copied.DeletedAt {
		t.Fatal("trash timestamp aliases original")
	}
}

// New mutable fields require an explicit review of the clone implementation.
func TestStorageCloneReferenceFieldInventory(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeOf(Job{}), []string{"SharedWith", "DeletedAt", "Subscription", "RequiredTier"}},
		{reflect.TypeOf(AuthorImport{}), []string{"Subscription"}},
		{reflect.TypeOf(CapturedSubscription{}), nil},
		{reflect.TypeOf(boosty.PostRequirement{}), nil},
	} {
		var fields []string
		for i := 0; i < tc.typ.NumField(); i++ {
			field := tc.typ.Field(i)
			switch field.Type.Kind() {
			case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer:
				fields = append(fields, field.Name)
			}
		}
		if !reflect.DeepEqual(fields, tc.want) {
			t.Fatalf("%s reference fields changed: %v; review clone", tc.typ.Name(), fields)
		}
	}
}
