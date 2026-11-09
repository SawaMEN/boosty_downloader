package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// writeArchiveJSON writes private raw Boosty payloads with lossless Zstandard
// compression when useful. The canonical paths are either name.json or
// name.json.zst, never both after a successful write. The public HTML and
// manifest remain plain files, so a saved post opens without a decompressor.
// The server does not expose these raw payloads via the shared archive or ZIP.
func writeArchiveJSON(path string, value any, level int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if level > 0 && len(data) >= 2048 {
		if binary, lookErr := exec.LookPath("zstd"); lookErr == nil {
			cmd := exec.Command(binary, "-q", "-T1", fmt.Sprintf("-%d", level), "-c")
			cmd.Stdin = bytes.NewReader(data)
			compressed, compErr := cmd.Output()
			if compErr != nil {
				return fmt.Errorf("zstd не смог сжать метаданные: %w", compErr)
			}
			if len(compressed) < len(data)*95/100 {
				if err := writeAtomic(path+".zst", compressed); err != nil {
					return err
				}
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
				return nil
			}
		}
	}
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	if err := os.Remove(path + ".zst"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
