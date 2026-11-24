package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

var errMediaIncomplete = errors.New("CDN передал неполные данные")

type mediaHTTPError struct {
	status int
}

func (e *mediaHTTPError) Error() string {
	return fmt.Sprintf("CDN HTTP %d", e.status)
}

func mediaExpired(err error) bool {
	var status *mediaHTTPError
	return errors.As(err, &status) && (status.status == 401 || status.status == 403 || status.status == 410)
}

func mediaTemporary(err error) bool {
	var status *mediaHTTPError
	if errors.As(err, &status) {
		switch status.status {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	var network net.Error
	return errors.Is(err, errMediaIncomplete) || errors.Is(err, io.ErrUnexpectedEOF) ||
		(errors.As(err, &network) && network.Timeout())
}

const mediaAttemptLimit = 3

func waitMediaRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt+1) * 150 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryMediaSegment(ctx context.Context, fetch func() (int64, error)) (int64, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, err := fetch()
		if err == nil || !mediaTemporary(err) || attempt+1 == mediaAttemptLimit {
			return n, err
		}
		if err := waitMediaRetry(ctx, attempt); err != nil {
			return 0, err
		}
	}
}

// A new signed URL is accepted only for one unambiguous media ID in the
// same still-accessible post. Do not match by position, title or filename.
func renewedMediaURL(original, current *boosty.Post, media boosty.Media) (string, error) {
	if original == nil || current == nil || original.ID == "" || current.ID != original.ID ||
		!current.HasAccess || media.ID == "" {
		return "", errors.New("CDN: невозможно подтвердить доступ или идентичность медиа")
	}
	var match *boosty.Media
	for _, candidate := range boosty.ExtractMedia(current) {
		if candidate.ID != media.ID || candidate.Type != media.Type {
			continue
		}
		if match != nil {
			return "", errors.New("CDN: неоднозначный идентификатор медиа")
		}
		copy := candidate
		match = &copy
	}
	if match == nil || match.Size != media.Size || match.Title != media.Title {
		return "", errors.New("CDN: медиа изменилось после обновления ссылки")
	}
	if media.Type == "ok_video" && mediaVideoQuality(*match) != mediaVideoQuality(media) {
		return "", errors.New("CDN: качество видеодорожки изменилось")
	}
	raw := boosty.MediaURL(current, *match)
	if raw == "" || raw == boosty.MediaURL(original, media) {
		return "", errors.New("CDN: новая ссылка не получена")
	}
	return raw, nil
}

func mediaVideoQuality(media boosty.Media) string {
	raw := boosty.BestVideo(media)
	for _, item := range media.PlayerURLs {
		if item.URL == raw {
			return item.Type
		}
	}
	return ""
}

type mediaPostRefresh func(context.Context) (*boosty.Post, error)

// Retry direct files in the same staging directory (Range/If-Range).
// Segment downloads have their own retries. On authorization failure, renew
// the post once and start the affected media from scratch, never append bytes
// from different signed URLs. Already completed sibling assets are untouched.
func downloadMediaWithRecovery(ctx context.Context, fetch mediaFetcher, refresh mediaPostRefresh, post *boosty.Post, media boosty.Media, dest string, expected int64) (int64, string, error) {
	raw := boosty.MediaURL(post, media)
	refreshed := false
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		var n int64
		var digest string
		var err error
		if media.Type == "ok_video" {
			n, digest, err = downloadVideoFile(ctx, fetch, raw, dest)
		} else {
			n, digest, err = downloadFile(ctx, fetch, raw, dest, expected)
		}
		if err == nil {
			return n, digest, nil
		}
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		if mediaExpired(err) && !refreshed && refresh != nil {
			refreshed = true
			current, refreshErr := refresh(ctx)
			if refreshErr != nil {
				return 0, "", errors.New("CDN: не удалось обновить данные поста")
			}
			next, refreshErr := renewedMediaURL(post, current, media)
			if refreshErr != nil {
				return 0, "", refreshErr
			}
			for _, suffix := range []string{".part", ".part.meta"} {
				if e := os.Remove(dest + suffix); e != nil && !os.IsNotExist(e) {
					return 0, "", e
				}
			}
			raw = next
			attempt = -1 // one independent bounded retry budget for the new URL
			continue
		}
		if !mediaTemporary(err) || attempt+1 >= mediaAttemptLimit {
			return 0, "", err
		}
		if err := waitMediaRetry(ctx, attempt); err != nil {
			return 0, "", err
		}
	}
}
