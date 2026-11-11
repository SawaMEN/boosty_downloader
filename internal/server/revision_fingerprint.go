package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// archiveContentFingerprint identifies a rendered archive snapshot, not a
// Boosty post ID or a permission. Signed CDN URLs and observation timestamps
// must never create a new revision of otherwise identical content.
func archiveContentFingerprint(man ArchiveManifest, blocks []archiveBlock, comments []commentView) (string, error) {
	type subscriptionIdentity struct {
		Level	string	`json:"level,omitempty"`
		LevelID	string	`json:"levelId,omitempty"`
		Price	int64	`json:"price,omitempty"`
		Paid	bool	`json:"paid"`
		Paused	bool	`json:"paused,omitempty"`
		Source	string	`json:"source"`
	}
	var tier *subscriptionIdentity
	if s := man.Subscription; s != nil {
		tier = &subscriptionIdentity{
			Level: s.Level, LevelID: s.LevelID, Price: s.Price,
			Paid: s.Paid, Paused: s.Paused, Source: s.Source,
		}
	}
	payload := struct {
		Title	string	`json:"title"`
		Author	string	`json:"author"`
		PostID	string	`json:"postId"`
		Source	string	`json:"source"`
		CommentsStatus	string	`json:"commentsStatus"`
		CommentCount	int	`json:"commentCount"`
		CommentExpected	int	`json:"commentExpected"`
		Failures	[]string	`json:"failures,omitempty"`
		Assets	[]ArchiveAsset	`json:"assets"`
		Blocks	[]archiveBlock	`json:"blocks"`
		Comments	[]commentView	`json:"comments"`
		RequiredTier	any	`json:"requiredTier,omitempty"`
		Subscription	*subscriptionIdentity	`json:"subscription,omitempty"`
	}{
		Title: man.Title, Author: man.Author, PostID: man.PostID,
		Source: man.Source, CommentsStatus: man.CommentsStatus,
		CommentCount: man.CommentCount, CommentExpected: man.CommentExpected,
		Failures: man.Failures, Assets: man.Assets,
		Blocks: blocks, Comments: comments, RequiredTier: man.RequiredTier,
		Subscription: tier,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

func validContentFingerprint(digest string) bool {
	return len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == ""
}

// discardIdenticalRevision removes ONLY the temporary previous snapshot made
// by the current refresh and ONLY after the new archive was fully published.
// Legacy snapshots without fingerprints remain untouched. Hardlinked media
// stay available through the current archive and every other owner/version.
func (s *Storage) discardIdenticalRevision(job Job, revisionID string) (bool, error) {
	if revisionID == "" {
		return false, nil
	}
	if !validRevisionID(revisionID) || !safeArchivePart(job.OwnerID) ||
		!safeArchivePart(job.Author) || !safeArchivePart(job.PostID) {
		return false, errors.New("неверный путь версии архива")
	}
	readManifest := func(path string) (ArchiveManifest, error) {
		var man ArchiveManifest
		data, err := os.ReadFile(path)
		if err != nil {
			return man, err
		}
		err = json.Unmarshal(data, &man)
		return man, err
	}
	previous, err := readManifest(filepath.Join(s.revisionsDir(job), revisionID, "manifest.json"))
	if err != nil {
		return false, err
	}
	current, err := readManifest(filepath.Join(s.archiveDir(job.OwnerID, job.Author, job.PostID), "manifest.json"))
	if err != nil {
		return false, err
	}
	if previous.Author != job.Author || previous.PostID != job.PostID ||
		current.Author != job.Author || current.PostID != job.PostID {
		return false, errors.New("версия не соответствует архиву")
	}
	if !validContentFingerprint(current.ContentSHA256) ||
		previous.ContentSHA256 != current.ContentSHA256 ||
		len(current.Failures) != 0 || len(previous.Failures) != 0 {
		return false, nil
	}
	if err := os.RemoveAll(filepath.Join(s.revisionsDir(job), revisionID)); err != nil {
		return false, err
	}
	return true, nil
}
