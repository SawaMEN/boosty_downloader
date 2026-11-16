package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
)

type AssetVerification struct {
	Filename string `json:"filename"`
	State    string `json:"state"`
	Size     int64  `json:"size"`
}
type ArchiveVerification struct {
	Valid     bool                `json:"valid"`
	Partial   bool                `json:"partial"`
	Verified  int                 `json:"verified"`
	Missing   int                 `json:"missing"`
	Damaged   int                 `json:"damaged"`
	Unchecked int                 `json:"unchecked"`
	Total     int                 `json:"total"`
	Items     []AssetVerification `json:"items"`
}

func verifyArchive(base string) (ArchiveVerification, error) {
	// The verification endpoint also must not follow archive symlinks.
	// Reading manifest.json via os.ReadFile would bypass the protections
	// already used by media downloads, shared ZIPs, and offline previews.
	_, manifest, err := readArchiveManifestNoFollow(base)
	if err != nil {
		return ArchiveVerification{}, err
	}
	result := ArchiveVerification{
		Valid: true,
		Partial: len(manifest.Failures) > 0 ||
			manifest.CommentsStatus == "частично" || manifest.CommentsStatus == "ошибка получения",
		Total: len(manifest.Assets), Items: []AssetVerification{},
	}
	for _, asset := range manifest.Assets {
		v := AssetVerification{Filename: asset.Filename, Size: asset.Size}
		if !safeArchivePart(asset.Filename) || asset.Size < 0 {
			v.State = "damaged"
		} else {
			// openArchiveRegular pins every directory and final file using
			// openat(O_NOFOLLOW); never Lstat then reopen a path for hashing.
			file, openErr := openArchiveRegular(base, "media/"+asset.Filename)
			switch {
			case errors.Is(openErr, os.ErrNotExist):
				v.State = "missing"
			case openErr != nil:
				v.State = "damaged"
			default:
				info, statErr := file.Stat()
				switch {
				case statErr != nil || info.Size() != asset.Size || info.Size() > maxArchiveMediaSize:
					v.State = "damaged"
				case asset.SHA256 == "":
					v.State = "unchecked"
				case len(asset.SHA256) != 64:
					v.State = "damaged"
				default:
					if _, err := hex.DecodeString(asset.SHA256); err != nil {
						v.State = "damaged"
					} else {
						// Only hash the already-open descriptor. A concurrent
						// rename cannot replace the bytes under this handle.
						digest := sha256.New()
						n, readErr := io.Copy(digest, io.LimitReader(file, maxArchiveMediaSize+1))
						if readErr != nil || n != asset.Size ||
							!strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), asset.SHA256) {
							v.State = "damaged"
						} else {
							v.State = "verified"
						}
					}
				}
				if closeErr := file.Close(); closeErr != nil {
					v.State = "damaged"
				}
			}
		}
		switch v.State {
		case "missing":
			result.Missing++
		case "damaged":
			result.Damaged++
		case "unchecked":
			result.Unchecked++
		case "verified":
			result.Verified++
		}
		result.Items = append(result.Items, v)
	}
	result.Valid = result.Missing == 0 && result.Damaged == 0 && result.Unchecked == 0
	return result, nil
}

// Explicit user action: hashes can be expensive for multi-gigabyte archives.
func (a *App) verifyJob(w http.ResponseWriter, r *http.Request) {
	j, ok := a.ownedJob(r)
	if !ok || (j.State != "done" && j.State != "partial") {
		fail(w, 404, "архив не найден")
		return
	}
	unlock, ready := a.archiveGates.tryRead(j)
	if !ready {
		fail(w, http.StatusConflict, "архив обновляется или восстанавливается")
		return
	}
	defer unlock()
	j, ok = a.ownedJob(r)
	if !ok || (j.State != "done" && j.State != "partial") {
		fail(w, 404, "архив не найден")
		return
	}
	result, err := verifyArchive(a.store.archiveDir(j.OwnerID, j.Author, j.PostID))
	if err != nil {
		fail(w, 500, "не удалось проверить целостность архива")
		return
	}
	reply(w, 200, result)
}
