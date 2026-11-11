// Package fsutil атомарно публикует файл: сначала копия во временное имя
// в каталоге назначения (тот же том, иначе rename не атомарен), затем замена.
package fsutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Install копирует src на место dst. Старый dst заменяется только после
// успешной записи временного файла. src не удаляется.
func Install(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".ferrytmp"
	if err := copySync(src, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := atomicRename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Move переносит файл. Если rename через границу тома невозможен, файл копируется.
func Move(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := Install(src, dst); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("copied to %s but failed to remove source: %w", dst, err)
	}
	return nil
}

// WriteAtomic записывает data в path через временный файл в том же каталоге.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".ferrytmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := atomicRename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func copySync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if syncErr != nil {
		_ = os.Remove(dst)
		return syncErr
	}
	return closeErr
}

func atomicRename(tmp, dst string) error {
	if err := os.Rename(tmp, dst); err == nil {
		return nil
	}
	// Windows не заменяет существующий файл через rename.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmp, dst)
}
