package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func TestArchiveMediaFatalStorageErrors(t *testing.T) {
	for _, err := range []error{syscall.ENOSPC, syscall.EDQUOT, context.Canceled, context.DeadlineExceeded, errStorageUserQuota, errStorageFreeReserve, errStorageBudgetProbe, errMediaMuxLimit} {
		if !fatalArchiveMediaError(fmt.Errorf("wrapped: %w", err)) {
			t.Fatalf("fatal error treated as missing asset: %v", err)
		}
	}
	if fatalArchiveMediaError(&mediaHTTPError{status: 404}) || fatalArchiveMediaError(errMediaIncomplete) || fatalArchiveMediaError(nil) {
		t.Fatal("ordinary remote error incorrectly fatal")
	}
}

// Use only the dedicated size-limited Linux tmpfs also used by the SQLite
// physical disk-full test. Never fill the runner or a VPS root filesystem.
func TestArchiveMediaPhysicalENOSPC(t *testing.T) {
	mount := os.Getenv("BD_TEST_SQLITE_TMPFS")
	if mount == "" {
		t.Skip("requires a dedicated size-limited tmpfs")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(mount, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != 0x01021994 || int64(fs.Blocks)*fs.Bsize > 64<<20 ||
		filepath.Clean(os.TempDir()) != filepath.Clean(mount) {
		t.Fatal("refusing to fill a filesystem that is not the isolated <=64 MiB tmpfs")
	}
	root, key, job, live := archiveUpdateFixture(t, true)
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := s.beginArchiveUpdate(job)
	if err != nil {
		t.Fatal(err)
	}
	old := make(map[string][]byte)
	for _, path := range []string{
		filepath.Join(root, "jobs.json"),
		filepath.Join(live, "manifest.json"),
		filepath.Join(live, "index.html"),
		filepath.Join(live, "media", "photo.jpg"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		old[path] = raw
	}
	fillerPath := filepath.Join(t.TempDir(), "fill")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer filler.Close()
	full := false
	chunk := make([]byte, 1<<20)
	for i := 0; i < 80; i++ {
		if _, err := filler.Write(chunk); errors.Is(err, syscall.ENOSPC) {
			full = true
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	if !full {
		t.Fatal("isolated tmpfs never reported ENOSPC")
	}
	info, err := os.Stat(fillerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(fillerPath, info.Size()-fs.Bsize); err != nil {
		t.Fatal(err)
	}
	// Leave a block for a real partial write, rather than only testing a
	// failed open. No validator means no sidecar consumes that block.
	payload := strings.Repeat("x", 2<<20)
	media := boosty.Media{ID: "photo", Type: "image", URL: "https://cdn.example/photo.jpg"}
	post := recoveryPost(t, media, true)
	fetch := func(context.Context, string, string, string) (*http.Response, error) {
		return fakeMedia(200, payload, nil, int64(len(payload))), nil
	}
	dest := filepath.Join(stage.Path, "media", "photo.jpg")
	_, _, err = downloadMediaWithRecovery(context.Background(), fetch, nil, post, media, dest, int64(len(payload)))
	if !errors.Is(err, syscall.ENOSPC) || !fatalArchiveMediaError(err) {
		t.Fatalf("direct media did not report fatal ENOSPC: %v", err)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("failed direct download retained partial bytes")
	}
	segment := filepath.Join(stage.Path, "media", "segment.m4s")
	if _, err := fetchHLSSegment(context.Background(), fetch, media.URL, segment, int64(len(payload))); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("segment did not report real ENOSPC: %v", err)
	}
	if _, err := os.Stat(segment); !os.IsNotExist(err) {
		t.Fatal("failed segment was retained")
	}
	for path, want := range old {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("disk-full changed published data: %s: %v", filepath.Base(path), err)
		}
	}
	if err := os.RemoveAll(stage.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	// Re-open storage as on restart, then retry a real media download and
	// publish a complete manifest through the archive transaction.
	s, err = newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	stage = archiveUpdateWriteStage(t, s, job)
	payload = "new media bytes"
	dest = filepath.Join(stage.Path, "media", "photo.jpg")
	if _, _, err := downloadMediaWithRecovery(context.Background(), fetch, nil, post, media, dest, int64(len(payload))); err != nil {
		t.Fatalf("retry after freeing space failed: %v", err)
	}
	ok, err := s.publishArchiveUpdate(job, stage)
	if err != nil || !ok {
		t.Fatalf("retry publication failed: %v %v", ok, err)
	}
	_, man, err := readArchiveManifestNoFollow(live)
	if err != nil || len(man.Assets) != 1 || man.Title != "New" {
		t.Fatalf("invalid retried archive: %+v %v", man, err)
	}
	valid, err := verifyAsset(filepath.Join(live, "media", "photo.jpg"), man.Assets[0])
	if err != nil || !valid {
		t.Fatalf("retried media failed SHA-256 check: %v", err)
	}
	got, ok := s.get(job.ID)
	if !ok || !got.SharedForAll || len(got.SharedWith) != 1 || got.SharedWith[0] != "member" || got.Title != "New" {
		t.Fatal("retry lost metadata or ACL")
	}
}

func TestMediaPolicyQuotaFailureIsFatal(t *testing.T) {
	root := t.TempDir()
	ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.initialized, b.used, b.maxUser = true, 3, 3
	payload := "video"
	media := boosty.Media{ID: "photo", Type: "image", URL: "https://cdn.example/photo.jpg"}
	post := recoveryPost(t, media, true)
	requests := 0
	fetch := func(context.Context, string, string, string) (*http.Response, error) {
		requests++
		return fakeMedia(200, payload, nil, int64(len(payload))), nil
	}
	dest := filepath.Join(root, "photo.jpg")
	_, _, err := downloadMediaWithRecovery(ctx, fetch, nil, post, media, dest, int64(len(payload)))
	if !errors.Is(err, errStorageUserQuota) || !fatalArchiveMediaError(err) {
		t.Fatalf("quota failure was not fatal: %v", err)
	}
	if requests != 1 {
		t.Fatalf("local quota retried as a remote failure: %d", requests)
	}
	for _, path := range []string{dest, dest + ".part", dest + ".part.meta"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("failed media left a file: %s: %v", filepath.Base(path), err)
		}
	}
	if b.used != 3 {
		t.Fatal("failed copy changed logical accounting")
	}
}

func TestMediaReserveAndProbeFailuresAreFatal(t *testing.T) {
	root := t.TempDir()
	_, free, err := filesystemBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withStorageBudget(context.Background(), root, "owner", 1, 0)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.minFree = free + 1
	if err := checkStorageBudget(ctx, 0); !errors.Is(err, errStorageFreeReserve) || !fatalArchiveMediaError(err) {
		t.Fatalf("reserve error not fatal: %v", err)
	}
	b.root = filepath.Join(root, "missing")
	if err := checkStorageBudget(ctx, 0); !errors.Is(err, errStorageBudgetProbe) || !fatalArchiveMediaError(err) {
		t.Fatalf("disk probe error not fatal: %v", err)
	}
}
