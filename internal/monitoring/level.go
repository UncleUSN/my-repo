package monitoring

import "strings"

const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// LevelOf задаёт уровень строки в alerts.log.
// Исчерпание попыток и сбои конфигурации — error, повтор и просрочка — warn.
func LevelOf(eventType string) string {
	switch eventType {
	case "failed", "delivery_failed", "config_invalid", "scenarios_reload_failed", "watcher_error":
		return LevelError
	case "retried", "delivery_overdue", "peer_unreachable", "alerts_dropped":
		return LevelWarn
	default:
		return LevelInfo
	}
}

// Allows сообщает, что строка уровня level проходит фильтр min.
func Allows(min, level string) bool {
	return rank(level) >= rank(min)
}

func rank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case LevelError:
		return 3
	case LevelWarn, "warning":
		return 2
	case LevelInfo, "":
		return 1
	default:
		return 1
	}
}
