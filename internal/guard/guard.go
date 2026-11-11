// Package guard проверяет, что локальный путь лежит внутри allowed_roots,
// и разворачивает символические ссылки, чтобы через них нельзя было выйти наружу.
package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/UncleUSN/my-repo/internal/paths"
)

// Guard — набор разрешённых корней. Пустой набор разрешает любой локальный путь;
// агент при этом пишет предупреждение в журнал.
type Guard struct {
	roots []string
	open  bool
}

// New нормализует корни. Каждый корень должен быть путём этой ОС.
func New(roots []string) (*Guard, error) {
	if len(roots) == 0 {
		return &Guard{open: true}, nil
	}
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		native, err := paths.ForLocalOS(root)
		if err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(native)
		if err != nil {
			return nil, err
		}
		abs = filepath.Clean(abs)
		// На Windows Temp из GitHub Actions — короткое имя RUNNER~1,
		// а EvalSymlinks раскрывает его в длинное. Храним обе формы:
		// входящий путь может быть любой из них.
		out = addUnique(out, abs)
		canon, err := resolveExisting(abs)
		if err != nil {
			return nil, err
		}
		out = addUnique(out, filepath.Clean(canon))
	}
	return &Guard{roots: out}, nil
}

// Resolve возвращает абсолютный путь без символических ссылок,
// гарантированно лежащий в одном из корней.
func (g *Guard) Resolve(p string) (string, error) {
	native, err := paths.ForLocalOS(p)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(native)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	resolved, err := resolveExisting(abs)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)
	if g.open || g.within(resolved) {
		return resolved, nil
	}
	// Лексически путь был внутри корня, а после раскрытия ссылок — нет.
	if g.within(abs) {
		return "", fmt.Errorf("path %q escapes allowed_roots through a symlink", p)
	}
	return "", fmt.Errorf("path %q is outside allowed_roots", p)
}

func (g *Guard) within(p string) bool {
	for _, root := range g.roots {
		ok, err := paths.WithinRoot(p, root)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// resolveExisting разворачивает ближайшего существующего предка.
// Несуществующий хвост (файл ещё не создан) дописывается как есть.
func resolveExisting(p string) (string, error) {
	var missing []string
	cur := p
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		missing = append([]string{filepath.Base(cur)}, missing...)
		cur = parent
	}
	resolved, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", err
	}
	if len(missing) == 0 {
		return resolved, nil
	}
	return filepath.Join(append([]string{resolved}, missing...)...), nil
}

// Roots возвращает нормализованные корни (для журнала и тестов).
func (g *Guard) Roots() []string {
	if g == nil {
		return nil
	}
	out := make([]string, len(g.roots))
	copy(out, g.roots)
	return out
}

// Open сообщает, что проверка корней отключена.
func (g *Guard) Open() bool {
	return g == nil || g.open
}

// Contains сообщает, что candidate лежит внутри root.
// Сначала сравнение строк, затем — после раскрытия коротких имён Windows и симлинков.
func Contains(root, candidate string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	if ok, err := paths.WithinRoot(candidate, root); err == nil && ok {
		return true
	}
	cr, err1 := resolveExisting(root)
	cc, err2 := resolveExisting(candidate)
	if err1 != nil || err2 != nil {
		return false
	}
	ok, err := paths.WithinRoot(filepath.Clean(cc), filepath.Clean(cr))
	return err == nil && ok
}

func addUnique(out []string, p string) []string {
	for _, existing := range out {
		if samePath(existing, p) {
			return out
		}
	}
	return append(out, p)
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
