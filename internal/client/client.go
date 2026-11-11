// Package client ходит на HTTPS API другого узла ferry.
// Большой файл режется на части: если связь оборвалась, повторный вызов
// продолжит с того смещения, которое сервер уже записал.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/UncleUSN/my-repo/internal/api"
	"github.com/UncleUSN/my-repo/internal/checksum"
	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/fsutil"
	"github.com/UncleUSN/my-repo/internal/ledger"
	"github.com/UncleUSN/my-repo/internal/store"
)

// DefaultChunkSize — размер одного PUT. Сервер принимает не больше 8 МиБ.
const DefaultChunkSize = 1 << 20

const maxChunkSize = 8 << 20

// Result — итог send/get.
type Result struct {
	Path          string
	Size          int64
	SHA256        string
	Resumed       bool
	AlreadyExists bool
}

// APIError — ответ сервера с кодом, отличным от успеха.
type APIError struct {
	Status  int
	Message string
	Offset  *int64
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("remote returned %d", e.Status)
	}
	return fmt.Sprintf("remote returned %d: %s", e.Status, e.Message)
}

// Client — HTTP-клиент одного узла.
type Client struct {
	// HTTP экспортирован, чтобы тест мог подменить транспорт и оборвать загрузку.
	HTTP  *http.Client
	base  string
	token string
	peer  string
	chunk int
}

// New собирает клиент по описанию peer из конфига.
func New(p config.Peer) (*Client, error) {
	transport, err := transportFor(p)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Token) == "" {
		return nil, fmt.Errorf("peer %s: token is empty", p.ID)
	}
	return &Client{
		HTTP:  &http.Client{Transport: transport},
		base:  strings.TrimRight(p.URL, "/"),
		token: p.Token,
		peer:  p.ID,
		chunk: DefaultChunkSize,
	}, nil
}

// DialLocal подключается к агенту на этой же машине.
// В качестве CA используется сертификат самого агента (он самоподписанный).
func DialLocal(cfg *config.Config) (*Client, error) {
	host, port, err := net.SplitHostPort(cfg.Node.Listen)
	if err != nil {
		return nil, fmt.Errorf("node.listen: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return New(config.Peer{
		ID:     cfg.Node.ID,
		URL:    "https://" + net.JoinHostPort(host, port),
		Token:  cfg.Node.Token,
		CAFile: cfg.Node.TLS.CertFile,
	})
}

// SetChunkSize меняет размер части. Нужен тестам и редким сетям.
func (c *Client) SetChunkSize(n int) {
	if n < 1 {
		return
	}
	if n > maxChunkSize {
		n = maxChunkSize
	}
	c.chunk = n
}

// WhoAmI проверяет токен и TLS.
func (c *Client) WhoAmI(ctx context.Context) (string, error) {
	var out api.WhoAmI
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/whoami", nil, &out); err != nil {
		return "", err
	}
	return out.Node, nil
}

// UploadFile отправляет локальный файл на remotePath.
// remotePath передаётся как есть: его разбирает узел-получатель.
func (c *Client) UploadFile(ctx context.Context, localPath, remotePath string) (Result, error) {
	return c.Upload(ctx, localPath, remotePath, "")
}

// Upload отправляет файл. alias — имя входящей папки на получателе;
// тогда remotePath относительный и не должен быть абсолютным.
func (c *Client) Upload(ctx context.Context, localPath, remotePath, alias string) (Result, error) {
	sum, size, err := hashFile(ctx, localPath)
	if err != nil {
		return Result{}, err
	}
	status, err := c.createUpload(ctx, api.CreateUploadRequest{
		Path:      remotePath,
		Alias:     alias,
		Size:      size,
		SHA256:    sum,
		Overwrite: true,
	})
	if err != nil {
		return Result{}, err
	}
	if status.Complete {
		return Result{Path: status.Path, Size: size, SHA256: sum, AlreadyExists: true}, nil
	}
	resumed := status.Offset > 0
	f, err := os.Open(localPath)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	if _, err := f.Seek(status.Offset, io.SeekStart); err != nil {
		return Result{}, err
	}
	buf := make([]byte, c.chunk)
	offset := status.Offset
	retries := 0
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			newOffset, putErr := c.putChunk(ctx, status.ID, offset, buf[:n])
			if apiErr, ok := putErr.(*APIError); ok && apiErr.Status == http.StatusConflict && apiErr.Offset != nil && retries < 3 {
				retries++
				offset = *apiErr.Offset
				if _, err := f.Seek(offset, io.SeekStart); err != nil {
					return Result{}, err
				}
				continue
			}
			if putErr != nil {
				return Result{}, putErr
			}
			offset = newOffset
			retries = 0
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Result{}, rerr
		}
	}
	committed, err := c.commit(ctx, status.ID)
	if err != nil {
		return Result{}, err
	}
	return Result{Path: committed.Path, Size: size, SHA256: sum, Resumed: resumed}, nil
}

// DownloadFile скачивает remotePath в localPath и сверяет SHA-256.
func (c *Client) DownloadFile(ctx context.Context, remotePath, localPath string) (Result, error) {
	info, err := c.Checksum(ctx, remotePath)
	if err != nil {
		return Result{}, err
	}
	if st, err := os.Stat(localPath); err == nil && !st.IsDir() && st.Size() == info.Size {
		sum, _, herr := checksum.File(localPath)
		if herr == nil && checksum.Equal(sum, info.SHA256) {
			return Result{Path: localPath, Size: info.Size, SHA256: info.SHA256, AlreadyExists: true}, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return Result{}, err
	}
	part := localPath + ".ferrypart"
	metaPath := localPath + ".ferrymeta"
	offset := int64(0)
	if metaMatches(metaPath, info.SHA256, info.Size, remotePath) {
		if st, err := os.Stat(part); err == nil && st.Size() <= info.Size {
			offset = st.Size()
		}
	} else {
		_ = os.Remove(part)
	}
	if err := writeMeta(metaPath, partMeta{SHA256: info.SHA256, Size: info.Size, Remote: remotePath}); err != nil {
		return Result{}, err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return Result{}, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return Result{}, err
		}
	}
	if offset < info.Size || info.Size == 0 && offset == 0 {
		if err := c.fetch(ctx, remotePath, offset, f); err != nil {
			f.Close()
			return Result{}, err
		}
	}
	if err := f.Close(); err != nil {
		return Result{}, err
	}
	sum, n, err := checksum.File(part)
	if err != nil {
		return Result{}, err
	}
	if n != info.Size || !checksum.Equal(sum, info.SHA256) {
		_ = os.Remove(part)
		_ = os.Remove(metaPath)
		return Result{}, fmt.Errorf("checksum mismatch for %s", remotePath)
	}
	if err := fsutil.Install(part, localPath); err != nil {
		return Result{}, err
	}
	_ = os.Remove(part)
	_ = os.Remove(metaPath)
	return Result{Path: localPath, Size: n, SHA256: sum, Resumed: offset > 0}, nil
}

// List возвращает каталог на удалённом узле.
func (c *Client) List(ctx context.Context, path string) (api.ListResponse, error) {
	var out api.ListResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/fs/list?path="+url.QueryEscape(path), nil, &out)
	return out, err
}

// Checksum запрашивает SHA-256 и размер.
func (c *Client) Checksum(ctx context.Context, path string) (api.ChecksumResponse, error) {
	var out api.ChecksumResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/fs/checksum?path="+url.QueryEscape(path), nil, &out)
	return out, err
}

// Jobs — список заданий локального или удалённого агента.
func (c *Client) Jobs(ctx context.Context) ([]api.JobInfo, error) {
	var out api.JobsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/jobs", nil, &out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// RunJob запускает задание и ждёт окончания.
// Если часть файлов не передалась, сервер отвечает 500, но тело — это итог, а не короткая ошибка.
func (c *Client) RunJob(ctx context.Context, name string) (api.RunResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/jobs/"+url.PathEscape(name)+"/run", nil)
	if err != nil {
		return api.RunResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return api.RunResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return api.RunResponse{}, err
	}
	var out api.RunResponse
	if json.Unmarshal(body, &out) == nil && (out.Name != "" || out.Failed > 0 || out.Transferred > 0) {
		if resp.StatusCode >= 300 || out.Failed > 0 {
			msg := strings.Join(out.Errors, "; ")
			if msg == "" {
				msg = resp.Status
			}
			return out, fmt.Errorf("job %s: %s", name, msg)
		}
		return out, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return api.RunResponse{}, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return api.RunResponse{}, err
	}
	return out, nil
}

// Deliveries — журнал доставки по назначениям.
func (c *Client) Deliveries(ctx context.Context, scenario, status string, limit int) ([]ledger.Record, error) {
	q := url.Values{}
	if scenario != "" {
		q.Set("scenario", scenario)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/deliveries"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out struct {
		Deliveries []ledger.Record `json:"deliveries"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Deliveries, nil
}

// ReloadScenarios просит агент перечитать YAML сценариев.
func (c *Client) ReloadScenarios(ctx context.Context) (int, error) {
	var out struct {
		Scenarios int `json:"scenarios"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/scenarios/reload", nil, &out); err != nil {
		return 0, err
	}
	return out.Scenarios, nil
}

// Transfers возвращает историю.
func (c *Client) Transfers(ctx context.Context, limit int) ([]store.Transfer, error) {
	path := "/api/v1/transfers"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out api.TransfersResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Transfers, nil
}

func (c *Client) createUpload(ctx context.Context, req api.CreateUploadRequest) (api.UploadStatus, error) {
	var out api.UploadStatus
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/uploads", req, &out)
	return out, err
}

func (c *Client) commit(ctx context.Context, id string) (api.UploadStatus, error) {
	var out api.UploadStatus
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/uploads/"+url.PathEscape(id)+"/commit", nil, &out)
	return out, err
}

func (c *Client) putChunk(ctx context.Context, id string, offset int64, chunk []byte) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/api/v1/uploads/"+url.PathEscape(id), bytes.NewReader(chunk))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Offset", strconv.FormatInt(offset, 10))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, readAPIError(resp)
	}
	var out api.UploadStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Offset, nil
}

func (c *Client) fetch(ctx context.Context, remote string, offset int64, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/fs/download?path="+url.QueryEscape(remote), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if offset > 0 && resp.StatusCode == http.StatusOK {
		return fmt.Errorf("server ignored Range, resume is not possible")
	}
	if offset > 0 && resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return readAPIError(resp)
	}
	if offset == 0 && resp.StatusCode != http.StatusOK {
		return readAPIError(resp)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return err
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func readAPIError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var er api.ErrorResponse
	if json.Unmarshal(b, &er) == nil && er.Error != "" {
		return &APIError{Status: resp.StatusCode, Message: er.Error, Offset: er.Offset}
	}
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}

func hashFile(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return "", n, err
		}
		nr, rerr := f.Read(buf)
		if nr > 0 {
			n += int64(nr)
			if _, err := h.Write(buf[:nr]); err != nil {
				return "", n, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", n, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

type partMeta struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Remote string `json:"remote"`
}

func metaMatches(path, sum string, size int64, remote string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m partMeta
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	return checksum.Equal(m.SHA256, sum) && m.Size == size && m.Remote == remote
}

func writeMeta(path string, m partMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
