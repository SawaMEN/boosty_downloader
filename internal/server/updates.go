package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
)

// The web service only stores policy and reads the status left by a separate
// root-owned host updater. It never executes arbitrary shell commands or
// receives a Docker socket, systemd privileges, or GitHub credentials.
type AutoUpdatePolicy struct {
	Channel    string `json:"channel"`
	CheckHours int    `json:"checkHours"`
	AutoApply  bool   `json:"autoApply"`
}

func defaultAutoUpdatePolicy() AutoUpdatePolicy {
	return AutoUpdatePolicy{Channel: "stable", CheckHours: 24, AutoApply: false}
}

func validateAutoUpdatePolicy(p AutoUpdatePolicy) error {
	if p.Channel != "stable" && p.Channel != "dev" {
		return errors.New("канал обновлений: stable или dev")
	}
	if p.CheckHours < 1 || p.CheckHours > 168 {
		return errors.New("интервал проверки обновлений: от 1 до 168 часов")
	}
	return nil
}

type updateStatus struct {
	CheckedAt string `json:"checkedAt"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
	Channel   string `json:"channel"`
	Latest    string `json:"latest"`
	Installed string `json:"installed"`
	AutoApply bool   `json:"autoApply"`
}

func readUpdateStatus(path string) (updateStatus, error) {
	var result updateStatus
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return updateStatus{State: "not_checked", Detail: "Служба проверки обновлений ещё не запускалась"}, nil
	}
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return result, errors.New("неверный размер или тип журнала обновлений")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return updateStatus{}, errors.New("повреждён журнал обновлений")
	}
	if len(result.Detail) > 1024 || len(result.Latest) > 128 || len(result.Installed) > 128 {
		return updateStatus{}, errors.New("журнал обновлений содержит слишком длинные поля")
	}
	return result, nil
}

func (a *App) adminUpdates(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	status, err := readUpdateStatus(filepath.Join(a.store.root, "update-status.json"))
	if err != nil {
		fail(w, 500, "не удалось прочитать журнал обновлений")
		return
	}
	reply(w, 200, map[string]any{"status": status, "policy": a.accounts.policy().Updates})
}
