package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
	var manifest ArchiveManifest
	b, err := os.ReadFile(filepath.Join(base, "manifest.json"))
	if err != nil {
		return ArchiveVerification{}, err
	}
	if err = json.Unmarshal(b, &manifest); err != nil {
		return ArchiveVerification{}, err
	}
	result := ArchiveVerification{Valid: true, Partial: len(manifest.Failures) > 0 || manifest.CommentsStatus == "частично" || manifest.CommentsStatus == "ошибка получения", Total: len(manifest.Assets), Items: []AssetVerification{}}
	for _, asset := range manifest.Assets {
		v := AssetVerification{Filename: asset.Filename, Size: asset.Size}
		switch {
		case !safeArchivePart(asset.Filename):
			v.State = "damaged"
			result.Damaged++
		case asset.SHA256 == "":
			if info, e := os.Lstat(filepath.Join(base, "media", asset.Filename)); os.IsNotExist(e) {
				v.State = "missing"
				result.Missing++
			} else if e != nil || !info.Mode().IsRegular() || info.Size() != asset.Size {
				v.State = "damaged"
				result.Damaged++
			} else {
				v.State = "unchecked"
				result.Unchecked++
			}
		default:
			ok, e := verifyAsset(filepath.Join(base, "media", asset.Filename), asset)
			if e != nil {
				return ArchiveVerification{}, e
			}
			if ok {
				v.State = "verified"
				result.Verified++
			} else if _, e := os.Lstat(filepath.Join(base, "media", asset.Filename)); os.IsNotExist(e) {
				v.State = "missing"
				result.Missing++
			} else {
				v.State = "damaged"
				result.Damaged++
			}
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
	result, err := verifyArchive(a.store.archiveDir(j.OwnerID, j.Author, j.PostID))
	if err != nil {
		fail(w, 500, "не удалось проверить целостность архива")
		return
	}
	reply(w, 200, result)
}
