// Package api содержит JSON-структуры HTTP API.
// Их используют и сервер, и CLI-клиент, чтобы имена полей не разъехались.
package api

import (
	"time"

	"github.com/UncleUSN/my-repo/internal/store"
)

// ErrorResponse — ошибка API.
type ErrorResponse struct {
	Error  string `json:"error"`
	Offset *int64 `json:"offset,omitempty"`
}

// Health — ответ без авторизации, чтобы проверить, что порт жив.
type Health struct {
	Status  string `json:"status"`
	Node    string `json:"node"`
	Version string `json:"version,omitempty"`
}

// WhoAmI — ответ с авторизацией: токен подошёл.
type WhoAmI struct {
	Node string `json:"node"`
}

// CreateUploadRequest начинает или продолжает загрузку.
type CreateUploadRequest struct {
	Path      string `json:"path"`
	Alias     string `json:"alias,omitempty"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Overwrite bool   `json:"overwrite"`
}

// UploadStatus — состояние сессии или уже лежащего файла.
type UploadStatus struct {
	ID       string `json:"id,omitempty"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Complete bool   `json:"complete"`
	Path     string `json:"path,omitempty"`
}

// Entry — элемент каталога.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// ListResponse — содержимое каталога.
type ListResponse struct {
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
}

// ChecksumResponse — контрольная сумма файла на узле.
type ChecksumResponse struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// JobInfo — краткое состояние задания.
type JobInfo struct {
	Name            string     `json:"name"`
	Kind            string     `json:"kind"`
	Schedule        string     `json:"schedule,omitempty"`
	Watch           string     `json:"watch,omitempty"`
	Source          string     `json:"source,omitempty"`
	DestinationNode string     `json:"destination_node,omitempty"`
	DestinationPath string     `json:"destination_path"`
	Running         bool       `json:"running"`
	LastRun         *time.Time `json:"last_run,omitempty"`
	LastStatus      string     `json:"last_status,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
}

// JobsResponse — список заданий.
type JobsResponse struct {
	Jobs []JobInfo `json:"jobs"`
}

// RunResponse — итог ручного запуска.
type RunResponse struct {
	Name        string   `json:"name"`
	Transferred int      `json:"transferred"`
	Failed      int      `json:"failed"`
	Errors      []string `json:"errors,omitempty"`
}

// TransfersResponse — история передач, новые записи первыми.
type TransfersResponse struct {
	Transfers []store.Transfer `json:"transfers"`
}
