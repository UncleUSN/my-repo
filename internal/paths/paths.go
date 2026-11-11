// Package paths приводит пути Windows и POSIX к каноническому виду
// и проверяет, что путь не выходит за каталог-корень.
//
// Правила не зависят от ОС, на которой собран бинарник: узел в WSL должен
// отвергнуть путь вида C:\..., а узел на Windows — абсолютный путь /home/...
// Сравнение каталогов Windows нечувствительно к регистру.
package paths

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// Kind — стиль пути, определённый по самой строке, а не по ОС процесса.
type Kind int

const (
	// POSIX — абсолютный или относительный путь со слэшами (/home/a, inbox/a.txt).
	POSIX Kind = iota
	// Windows — диск (C:\...), UNC (\\server\share) или относительный путь с обратными слэшами.
	Windows
)

const maxPathLen = 32767

// Detect определяет стиль пути по его тексту.
// Относительный путь без обратных слэшей считается POSIX; вызывающий код
// на Windows приводит его к локальным разделителям в ForLocalOS.
func Detect(p string) Kind {
	if strings.HasPrefix(p, `\\`) {
		return Windows
	}
	if len(p) >= 2 && isLetter(p[0]) && p[1] == ':' {
		return Windows
	}
	if strings.Contains(p, `\`) && !strings.Contains(p, "/") {
		return Windows
	}
	return POSIX
}

// IsAbs сообщает, является ли путь абсолютным в своём стиле.
// «C:foo» (путь относительно текущего каталога диска) абсолютным не считается.
func IsAbs(p string) bool {
	switch Detect(p) {
	case Windows:
		if strings.HasPrefix(p, `\\`) {
			return true
		}
		return len(p) >= 3 && isLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
	default:
		return strings.HasPrefix(p, "/")
	}
}

// HasTrailingSeparator сообщает, что путь явно указывает на каталог.
func HasTrailingSeparator(p string) bool {
	return strings.HasSuffix(p, "/") || strings.HasSuffix(p, `\`)
}

// Clean приводит путь к каноническому виду в его собственном стиле.
// Для Windows разделитель на выходе всегда «\», буква диска — заглавная.
func Clean(p string) (string, error) {
	cleaned, _, err := clean(p)
	return cleaned, err
}

func clean(p string) (string, Kind, error) {
	if err := validateRaw(p); err != nil {
		return "", 0, err
	}
	p = stripExtendedPrefix(p)
	kind := Detect(p)
	if kind == Windows {
		out, err := cleanWindows(p)
		return out, Windows, err
	}
	if p == "" {
		return "", POSIX, fmt.Errorf("empty path")
	}
	return path.Clean(p), POSIX, nil
}

func validateRaw(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if len(p) > maxPathLen {
		return fmt.Errorf("path is too long")
	}
	if strings.ContainsRune(p, 0) || strings.ContainsAny(p, "\r\n") {
		return fmt.Errorf("path contains a forbidden character")
	}
	if !utf8.ValidString(p) {
		return fmt.Errorf("path is not valid UTF-8")
	}
	return nil
}

// stripExtendedPrefix снимает префикс \\?\ который Проводник иногда добавляет.
func stripExtendedPrefix(p string) string {
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(p, `\\?\UNC\`)
	}
	if strings.HasPrefix(p, `\\?\`) {
		return strings.TrimPrefix(p, `\\?\`)
	}
	return p
}

func cleanWindows(p string) (string, error) {
	p = strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(p, `\\`) {
		parts := splitWindows(strings.TrimPrefix(p, `\\`))
		if len(parts) < 2 {
			return "", fmt.Errorf("invalid UNC path %q", p)
		}
		cleaned, err := cleanSegments(parts[2:], false)
		if err != nil {
			return "", fmt.Errorf("%w: %q", err, p)
		}
		out := `\\` + parts[0] + `\` + parts[1]
		if len(cleaned) > 0 {
			out += `\` + strings.Join(cleaned, `\`)
		}
		return out, nil
	}
	if len(p) >= 2 && isLetter(p[0]) && p[1] == ':' {
		vol := strings.ToUpper(p[:2])
		rest := ""
		if len(p) > 2 {
			rest = p[2:]
		}
		if rest == "" {
			return vol + `\`, nil
		}
		if !strings.HasPrefix(rest, `\`) {
			return "", fmt.Errorf("drive-relative path %q is not supported", p)
		}
		cleaned, err := cleanSegments(splitWindows(rest), false)
		if err != nil {
			return "", fmt.Errorf("%w: %q", err, p)
		}
		if len(cleaned) == 0 {
			return vol + `\`, nil
		}
		return vol + `\` + strings.Join(cleaned, `\`), nil
	}
	cleaned, err := cleanSegments(splitWindows(p), true)
	if err != nil {
		return "", err
	}
	if len(cleaned) == 0 {
		return ".", nil
	}
	return strings.Join(cleaned, `\`), nil
}

func splitWindows(p string) []string {
	raw := strings.Split(p, `\`)
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func cleanSegments(parts []string, allowAbove bool) ([]string, error) {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case ".":
			continue
		case "..":
			if len(out) == 0 || out[len(out)-1] == ".." {
				if !allowAbove {
					return nil, fmt.Errorf("path escapes root")
				}
				out = append(out, "..")
				continue
			}
			out = out[:len(out)-1]
		default:
			out = append(out, part)
		}
	}
	return out, nil
}

// WithinRoot сообщает, что path лежит внутри root либо совпадает с ним.
// Оба пути приводятся через Clean. Для Windows сравнение без учёта регистра,
// граница каталога уважается: C:\ferry не содержит C:\ferry2.
func WithinRoot(pathStr, root string) (bool, error) {
	cp, ck, err := clean(pathStr)
	if err != nil {
		return false, err
	}
	cr, rk, err := clean(root)
	if err != nil {
		return false, err
	}
	if ck != rk {
		return false, nil
	}
	if ck == Windows {
		cp = strings.ToLower(cp)
		cr = strings.ToLower(cr)
		if cr == `\` || strings.HasSuffix(cr, `:`) {
			// Корень диска без слэша сюда не попадает: Clean даёт «C:\».
		}
		if !strings.HasSuffix(cr, `\`) {
			cr += `\`
		}
		if !strings.HasSuffix(cp, `\`) {
			cpCheck := cp + `\`
			return cpCheck == cr || strings.HasPrefix(cpCheck, cr), nil
		}
		return cp == cr, nil
	}
	if cr != "/" && !strings.HasSuffix(cr, "/") {
		cr += "/"
	}
	if cp != "/" && !strings.HasSuffix(cp, "/") {
		return cp+"/" == cr || strings.HasPrefix(cp+"/", cr), nil
	}
	return cp == cr, nil
}

// Join присоединяет относительный путь к каталогу в стиле этого каталога.
// rel не может быть абсолютным и не может содержать «..».
func Join(dir, rel string) (string, error) {
	rel = strings.ReplaceAll(rel, `\`, "/")
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return Clean(dir)
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid relative path %q", rel)
		}
	}
	cleaned, kind, err := clean(dir)
	if err != nil {
		return "", err
	}
	if kind == Windows {
		base := strings.TrimRight(cleaned, `\`)
		// «C:» после TrimRight у корня диска «C:\» — вернём слэш сами.
		if len(base) == 2 && base[1] == ':' {
			base += `\`
			return base + strings.Join(parts, `\`), nil
		}
		return base + `\` + strings.Join(parts, `\`), nil
	}
	if cleaned == "/" {
		return "/" + strings.Join(parts, "/"), nil
	}
	return strings.TrimRight(cleaned, "/") + "/" + strings.Join(parts, "/"), nil
}

// Match сопоставляет имя с glob-шаблоном.
// Если в шаблоне нет «/», он применяется к базовому имени, поэтому «*.log»
// совпадает с «sub/app.log». Звёздочка не переходит через «/».
func Match(pattern, rel string) (bool, error) {
	if pattern == "" {
		pattern = "*"
	}
	pattern = strings.ReplaceAll(pattern, `\`, "/")
	rel = strings.ReplaceAll(rel, `\`, "/")
	rel = strings.TrimPrefix(rel, "./")
	if !strings.Contains(pattern, "/") {
		rel = path.Base(rel)
	}
	return path.Match(pattern, rel)
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
