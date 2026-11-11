package paths

import (
	"fmt"
	"runtime"
	"strings"
)

// ForLocalOS возвращает путь, который можно передать в os.Open на этой ОС.
// Чужой абсолютный стиль отвергается: Windows-путь на агенте в WSL — это
// ошибка конфигурации, а не повод молча писать файл с странным именем.
func ForLocalOS(p string) (string, error) {
	cleaned, kind, err := clean(p)
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "windows":
		if kind == POSIX && strings.HasPrefix(cleaned, "/") {
			return "", fmt.Errorf("POSIX absolute path %q cannot be used on Windows", p)
		}
		return strings.ReplaceAll(cleaned, "/", `\`), nil
	default:
		if kind == Windows {
			return "", fmt.Errorf("Windows path %q cannot be used on %s", p, runtime.GOOS)
		}
		return cleaned, nil
	}
}
