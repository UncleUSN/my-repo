package jobs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UncleUSN/my-repo/internal/guard"
	"github.com/UncleUSN/my-repo/internal/paths"
)

const maxFiles = 10000

// File — локальный файл, который задание готово отправить.
type File struct {
	Abs     string
	Rel     string
	Size    int64
	Mod     time.Time
	FromDir bool
}

// collect обходит источник. include — хотя бы один шаблон должен совпасть,
// exclude отбрасывает. Символические ссылки пропускаются.
func collect(root string, include, exclude []string, recursive bool, archive string) ([]File, bool, error) {
	if len(include) == 0 {
		include = []string{"*"}
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%s is a symlink, refusing to follow it", root)
	}
	if !info.IsDir() {
		if isTemp(info.Name()) {
			return nil, false, nil
		}
		ok, err := matchFilters(info.Name(), include, exclude)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		return []File{{
			Abs:  root,
			Rel:  info.Name(),
			Size: info.Size(),
			Mod:  info.ModTime(),
		}}, false, nil
	}

	var out []File
	add := func(abs string, d fs.DirEntry) error {
		if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
			return nil
		}
		if isTemp(d.Name()) {
			return nil
		}
		if archive != "" && guard.Contains(archive, abs) {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("file %s is outside %s", abs, root)
		}
		rel = filepath.ToSlash(rel)
		ok, err := matchFilters(rel, include, exclude)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, File{Abs: abs, Rel: rel, Size: info.Size(), Mod: info.ModTime(), FromDir: true})
		if len(out) > maxFiles {
			return fmt.Errorf("more than %d files matched in %s", maxFiles, root)
		}
		return nil
	}

	if !recursive {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, true, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := add(filepath.Join(root, e.Name()), e); err != nil {
				return nil, true, err
			}
		}
		return out, true, nil
	}

	err = filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if abs == root {
			return nil
		}
		if d.IsDir() {
			if archive != "" && guard.Contains(archive, abs) {
				return fs.SkipDir
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fs.SkipDir
			}
			return nil
		}
		return add(abs, d)
	})
	if err != nil {
		return nil, true, err
	}
	return out, true, nil
}

func matchFilters(rel string, include, exclude []string) (bool, error) {
	okAny := false
	for _, pattern := range include {
		ok, err := paths.Match(pattern, rel)
		if err != nil {
			return false, err
		}
		if ok {
			okAny = true
			break
		}
	}
	if !okAny {
		return false, nil
	}
	for _, pattern := range exclude {
		ok, err := paths.Match(pattern, rel)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}
	return true, nil
}

func isTemp(name string) bool {
	base := filepath.Base(name)
	return strings.HasSuffix(base, ".ferrypart") ||
		strings.HasSuffix(base, ".ferrytmp") ||
		strings.HasSuffix(base, ".ferrymeta")
}
