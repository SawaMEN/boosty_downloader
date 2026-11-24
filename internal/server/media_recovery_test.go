package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func recoveryPost(t *testing.T, media boosty.Media, access bool) *boosty.Post {
	t.Helper()
	raw, err := json.Marshal(media)
	if err != nil {
		t.Fatal(err)
	}
	return &boosty.Post{ID: "post", HasAccess: access, Data: []json.RawMessage{raw}}
}

func TestMediaRecoveryRenewsOnlyMatchingAsset(t *testing.T) {
	for _, mode := range []string{"success", "denied", "changed", "duplicate", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			media := boosty.Media{ID: "one", Type: "file", Title: "file.txt", URL: "https://cdn.example/old", Size: 10}
			post := recoveryPost(t, media, true)
			fresh := media
			fresh.URL = "https://cdn.example/new"
			if mode == "changed" {
				fresh.Size++
			}
			if mode == "unchanged" {
				fresh.URL = media.URL
			}
			current := recoveryPost(t, fresh, mode != "denied")
			if mode == "duplicate" {
				current.Data = append(current.Data, current.Data[0])
			}
			calls, refreshes := 0, 0
			dest := filepath.Join(t.TempDir(), "file.txt")
			fetch := func(_ context.Context, raw, byteRange, ifRange string) (*http.Response, error) {
				calls++
				if calls == 1 {
					return fakeMedia(200, "hello", map[string]string{"ETag": "\"old\""}, 10), nil
				}
				if calls == 2 {
					if byteRange != "bytes=5-" || ifRange != "\"old\"" {
						t.Fatalf("resume lost: %s %s", byteRange, ifRange)
					}
					return fakeMedia(403, "", nil, 0), nil
				}
				if raw != fresh.URL || byteRange != "" || ifRange != "" {
					t.Fatal("old partial reused for new URL")
				}
				return fakeMedia(200, "newcontent", nil, 10), nil
			}
			n, digest, err := downloadMediaWithRecovery(context.Background(), fetch, func(context.Context) (*boosty.Post, error) {
				refreshes++
				return current, nil
			}, post, media, dest, 10)
			if refreshes != 1 {
				t.Fatalf("refresh count=%d", refreshes)
			}
			if mode == "success" {
				if err != nil || n != 10 || digest != shaText("newcontent") || calls != 3 {
					t.Fatalf("recovery failed: %d %s %v", n, digest, err)
				}
			} else if err == nil || calls != 2 {
				t.Fatalf("unsafe refreshed object accepted: calls=%d err=%v", calls, err)
			} else if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("failed recovery published a file")
			}
		})
	}
}

func TestSegmentRetryAfterShortBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segment")
	calls := 0
	fetch := func(context.Context, string, string, string) (*http.Response, error) {
		calls++
		if calls == 1 {
			return fakeMedia(200, "short", nil, 10), nil
		}
		return fakeMedia(200, "0123456789", nil, 10), nil
	}
	n, err := fetchHLSSegment(context.Background(), fetch, "https://cdn.example/seg", path, 100)
	if err != nil || n != 10 || calls != 2 {
		t.Fatalf("segment retry failed: %d %d %v", n, calls, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "0123456789" {
		t.Fatal("retry mixed segment bytes")
	}
}

func TestSegmentRetryDoesNotRetryPermanentFailure(t *testing.T) {
	calls := 0
	_, err := retryMediaSegment(context.Background(), func() (int64, error) {
		calls++
		return 0, &mediaHTTPError{status: 403}
	})
	if calls != 1 || !mediaExpired(err) {
		t.Fatalf("permanent error retried: %d %v", calls, err)
	}
}
