package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const gibibyte int64 = 1 << 30

type storageBudgetKey struct{}

// storageBudget applies a logical per-user quota to all archive bytes, including
// temporary files. Each archive worker owns a budget; failed writes never
// continue after the reserve is breached. No symlinks are followed.
type storageBudget struct {
	mu          sync.Mutex
	root        string
	owner       string
	minFree     int64
	maxUser     int64
	initialized bool
	used        int64
}

func withStorageBudget(ctx context.Context, root, owner string, minFreeGiB, maxUserGiB int) context.Context {
	if minFreeGiB == 0 && maxUserGiB == 0 {
		return ctx
	}
	return context.WithValue(ctx, storageBudgetKey{}, &storageBudget{
		root: root, owner: owner,
		minFree: int64(minFreeGiB) * gibibyte,
		maxUser: int64(maxUserGiB) * gibibyte,
	})
}

func userStoredBytes(root, owner string) (int64, error) {
	if !safeArchivePart(owner) {
		return 0, errors.New("неверный идентификатор владельца архива")
	}
	path := filepath.Join(root, "library", owner)
	var total int64
	err := filepath.WalkDir(path, func(_ string, item os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if item.Type()&os.ModeSymlink != 0 {
			return errors.New("символическая ссылка внутри архива запрещена")
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return errors.New("необычный объект внутри архива")
		}
		if total > int64(^uint64(0)>>1)-info.Size() {
			return errors.New("переполнение размера библиотеки")
		}
		total += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

func filesystemBytes(path string) (total, free int64, err error) {
	var s syscall.Statfs_t
	if err = syscall.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	return int64(s.Blocks) * int64(s.Bsize), int64(s.Bavail) * int64(s.Bsize), nil
}

func (b *storageBudget) checkLocked(extra int64) error {
	if extra < 0 {
		return errors.New("отрицательный размер записи")
	}
	if b.maxUser > 0 {
		if !b.initialized {
			used, err := userStoredBytes(b.root, b.owner)
			if err != nil {
				return err
			}
			b.used = used
			b.initialized = true
		}
		if extra > b.maxUser-b.used {
			return fmt.Errorf("превышена квота архива пользователя (%d ГиБ)", b.maxUser/gibibyte)
		}
	}
	if b.minFree > 0 {
		_, free, err := filesystemBytes(b.root)
		if err != nil {
			return err
		}
		if extra > free-b.minFree {
			return fmt.Errorf("на диске должен оставаться резерв %d ГиБ", b.minFree/gibibyte)
		}
	}
	return nil
}

func checkStorageBudget(ctx context.Context, additional int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checkLocked(additional)
}

// releaseStorageBudget credits a staging file only after its successful removal.
// It must not be used for files that remain in an archive.
func releaseStorageBudget(ctx context.Context, removed int64) {
	if removed <= 0 {
		return
	}
	b, _ := ctx.Value(storageBudgetKey{}).(*storageBudget)
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if removed > b.used {
		b.used = 0
	} else {
		b.used -= removed
	}
}

type guardedWriter struct {
	ctx context.Context
	out io.Writer
}

func (w guardedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	budget, _ := w.ctx.Value(storageBudgetKey{}).(*storageBudget)
	if budget == nil {
		return w.out.Write(p)
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if err := budget.checkLocked(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := w.out.Write(p)
	budget.used += int64(n)
	return n, err
}

// guardedCopy checks quotas *before* each bounded write. Unlike a single
// pre-download check it also protects unknown-length CDN responses.
func guardedCopy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	return io.CopyBuffer(guardedWriter{ctx: ctx, out: dst}, src, make([]byte, 256<<10))
}

type storageOwnerUsage struct {
	Username string `json:"username"`
	Bytes    int64  `json:"bytes"`
	Error    string `json:"error,omitempty"`
}

// Admin-only inventory. Logical owner sizes include hardlink-shared files;
// filesystem free bytes represent the physical volume's actual capacity.
func (a *App) adminStorage(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	total, free, err := filesystemBytes(a.store.root)
	if err != nil {
		fail(w, 500, "не удалось прочитать сведения о диске")
		return
	}
	owners := make([]storageOwnerUsage, 0)
	for _, account := range a.accounts.listUsers() {
		used, err := userStoredBytes(a.store.root, account.ID)
		entry := storageOwnerUsage{Username: account.Username, Bytes: used}
		if err != nil {
			entry.Error = "ошибка чтения каталога"
		}
		owners = append(owners, entry)
	}
	reply(w, 200, map[string]any{
		"totalBytes": total, "freeBytes": free, "users": owners,
		"minFreeGiB": a.accounts.policy().MinFreeGiB,
		"maxUserGiB": a.accounts.policy().MaxUserGiB,
	})
}