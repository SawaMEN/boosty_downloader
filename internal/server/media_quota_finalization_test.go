package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func TestDirectMediaRecoveryQuotaAfterReplacement(t *testing.T) {
	for _, mode := range []string{"replace", "renew", "failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
			b := ctx.Value(storageBudgetKey{}).(*storageBudget)
			b.maxUser = 10
			b.initialized = true
			media := boosty.Media{ID: "one", Type: "file", Title: "a.bin", URL: "https://cdn.example/old", Size: 10}
			post := recoveryPost(t, media, true)
			next := media
			next.URL = "https://cdn.example/new"
			fresh := recoveryPost(t, next, true)
			calls := 0
			fetch := func(_ context.Context, _ string, _, _ string) (*http.Response, error) {
				calls++
				if calls == 1 {
					return fakeMedia(200, "hello", map[string]string{"ETag": "\"v1\""}, 10), nil
				}
				if mode == "failure" {
					return fakeMedia(404, "", nil, 0), nil
				}
				if mode == "renew" && calls == 2 {
					return fakeMedia(403, "", nil, 0), nil
				}
				return fakeMedia(200, "0123456789", nil, 10), nil
			}
			dest := filepath.Join(root, "file.bin")
			n, _, err := downloadMediaWithRecovery(ctx, fetch, func(context.Context) (*boosty.Post, error) {
				return fresh, nil
			}, post, media, dest, 10)
			if mode == "failure" {
				if err == nil || b.used != 0 {
					t.Fatalf("failed download retained quota: %v %d", err, b.used)
				}
			} else if err != nil || n != 10 || b.used != 10 {
				t.Fatalf("replacement quota: n=%d used=%d err=%v", n, b.used, err)
			}
			if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
				t.Fatal("partial file was not removed")
			}
		})
	}
}

func TestIntermediateMediaOutputQuota(t *testing.T) {
	root := t.TempDir()
	ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.maxUser, b.used, b.initialized = 15, 3, true
	scoped := withMediaStageBudget(ctx)
	dir := t.TempDir()
	if err := chargeIntermediateMediaOutput(scoped, 8); err != nil {
		t.Fatal(err)
	}
	if err := chargeIntermediateMediaOutput(scoped, 5); err == nil {
		t.Fatal("intermediate exceeded quota")
	}
	if b.used != 11 {
		t.Fatalf("unexpected intermediate charge: %d", b.used)
	}
	cleanupMediaStage(scoped, dir)
	if b.used != 3 {
		t.Fatalf("intermediate charge leaked: %d", b.used)
	}
}
