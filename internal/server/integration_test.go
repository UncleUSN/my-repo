package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/UncleUSN/my-repo/internal/cert"
	"github.com/UncleUSN/my-repo/internal/client"
	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/server"
)

func TestHealthAndAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	agent, _ := startNode(t, ctx, nodeOpts{id: "a", token: longToken, root: root})

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	httpClient := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := httpClient.Get(agent.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health %d", resp.StatusCode)
	}

	cl := mustClient(t, agent.URL, "wrong-token-here", true, "")
	_, err = cl.WhoAmI(ctx)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", err)
	}
	ok, err := mustClient(t, agent.URL, longToken, true, "").WhoAmI(ctx)
	if err != nil || ok != "a" {
		t.Fatalf("whoami %q %v", ok, err)
	}
}

func TestPingInlineTokenBetweenNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	const token = "0123456789abcdef0123456789abcdef"
	bRoot := filepath.Join(dir, "b")
	aRoot := filepath.Join(dir, "a")
	for _, d := range []string{bRoot, aRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bCert, err := cert.Generate(cert.Options{Dir: filepath.Join(bRoot, "certs"), CommonName: "wsl"})
	if err != nil {
		t.Fatal(err)
	}
	aCert, err := cert.Generate(cert.Options{Dir: filepath.Join(aRoot, "certs"), CommonName: "win"})
	if err != nil {
		t.Fatal(err)
	}
	// В кавычках YAML \n — перевод строки. После загрузки он должен срезаться.
	bYAML := fmt.Sprintf(`
node:
  id: wsl
  listen: "127.0.0.1:0"
  token: "%s\n"
  data_dir: %q
  allowed_roots: [%q]
  tls:
    cert_file: %q
    key_file: %q
`, token, filepath.Join(bRoot, "data"), bRoot, bCert.CertFile, bCert.KeyFile)
	bPath := filepath.Join(bRoot, "ferry.yaml")
	if err := os.WriteFile(bPath, []byte(bYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	bCfg, err := config.Load(bPath)
	if err != nil {
		t.Fatal(err)
	}
	if bCfg.Node.Token != token {
		t.Fatalf("wsl token %q", bCfg.Node.Token)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	agentB, err := server.Start(ctx, bCfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentB.Close() })

	aYAML := fmt.Sprintf(`
node:
  id: win
  listen: "127.0.0.1:0"
  token: "ffffffffffffffffffffffffffffffff"
  data_dir: %q
  allowed_roots: [%q]
  tls:
    cert_file: %q
    key_file: %q
peers:
  - id: wsl
    url: %q
    token: "%s  "
    tls_skip_verify: true
`, filepath.Join(aRoot, "data"), aRoot, aCert.CertFile, aCert.KeyFile, agentB.URL, token)
	aPath := filepath.Join(aRoot, "ferry.yaml")
	if err := os.WriteFile(aPath, []byte(aYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	aCfg, err := config.Load(aPath)
	if err != nil {
		t.Fatal(err)
	}
	peer, ok := aCfg.Peer("wsl")
	if !ok {
		t.Fatal("missing peer")
	}
	if peer.Token != token {
		t.Fatalf("peer token %q", peer.Token)
	}
	cl, err := client.New(peer)
	if err != nil {
		t.Fatal(err)
	}
	id, err := cl.WhoAmI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != "wsl" {
		t.Fatalf("whoami %s", id)
	}
	bad, err := client.New(config.Peer{ID: "wsl", URL: agentB.URL, Token: "ffffeeeeffffeeeeffffeeeeffffeeee", TLSSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bad.WhoAmI(ctx)
	if err == nil || !strings.Contains(err.Error(), "node.token") {
		t.Fatalf("expected token hint, got %v", err)
	}
}

func TestTrustedCert(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	agent, certFile := startNode(t, ctx, nodeOpts{id: "a", token: longToken, root: root})
	cl := mustClient(t, agent.URL, longToken, false, certFile)
	id, err := cl.WhoAmI(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != "a" {
		t.Fatalf("id %s", id)
	}
}

func TestUploadDownloadBothWays(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rootA := t.TempDir()
	rootB := t.TempDir()
	agentA, _ := startNode(t, ctx, nodeOpts{id: "a", token: longToken, root: rootA})
	agentB, _ := startNode(t, ctx, nodeOpts{id: "b", token: longToken, root: rootB})

	payload := bytes.Repeat([]byte("hello ferry "), 1000)
	src := filepath.Join(rootA, "note.txt")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(rootB, "inbox", "note.txt")
	clB := mustClient(t, agentB.URL, longToken, true, "")
	res, err := clB.UploadFile(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != int64(len(payload)) || res.SHA256 == "" {
		t.Fatalf("result %+v", res)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("content mismatch")
	}

	back := filepath.Join(rootA, "back.txt")
	clA := mustClient(t, agentA.URL, longToken, true, "")
	if _, err := clA.DownloadFile(ctx, src, back); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(back)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("download mismatch %v", err)
	}

	// Обратное направление: клиент заливает файл уже на узел A.
	fromB := []byte("from b")
	srcB := filepath.Join(rootB, "from-b.txt")
	if err := os.WriteFile(srcB, fromB, 0o644); err != nil {
		t.Fatal(err)
	}
	onA := filepath.Join(rootA, "from-b.txt")
	if _, err := clA.UploadFile(ctx, srcB, onA); err != nil {
		t.Fatal(err)
	}
	if got, err = os.ReadFile(onA); err != nil || !bytes.Equal(got, fromB) {
		t.Fatalf("reverse upload: %v", err)
	}

	// Повторная отправка того же содержимого не переписывает байты зря.
	again, err := clB.UploadFile(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyExists {
		t.Fatal("expected short-circuit")
	}

	hist, err := clB.Transfers(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || hist[0].Status != "success" {
		t.Fatalf("history %+v", hist)
	}
}

func TestResumeUpload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	agent, _ := startNode(t, ctx, nodeOpts{id: "a", token: longToken, root: root})
	payload := bytes.Repeat([]byte("0123456789"), 500) // 5000 байт
	src := filepath.Join(root, "big.bin")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "out", "big.bin")
	cl := mustClient(t, agent.URL, longToken, true, "")
	cl.SetChunkSize(1024)
	base := cl.HTTP.Transport
	cl.HTTP.Transport = &failAfter{base: base, failOn: 2}
	if _, err := cl.UploadFile(ctx, src, dst); err == nil {
		t.Fatal("expected injected failure")
	}
	cl.HTTP.Transport = base
	res, err := cl.UploadFile(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resumed {
		t.Fatal("expected resume")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("len got %d want %d", len(got), len(payload))
	}
}

func TestForeignPathRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	agent, _ := startNode(t, ctx, nodeOpts{id: "a", token: longToken, root: root})
	src := filepath.Join(root, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := `C:\ferry\inbox\a.txt`
	if runtime.GOOS == "windows" {
		foreign = `/etc/passwd`
	}
	cl := mustClient(t, agent.URL, longToken, true, "")
	_, err := cl.UploadFile(ctx, src, foreign)
	if err == nil {
		t.Fatal("foreign path was accepted")
	}
}

func TestPathOutsideRoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	agent, _ := startNode(t, ctx, nodeOpts{id: "a", token: longToken, root: root})
	src := filepath.Join(root, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	cl := mustClient(t, agent.URL, longToken, true, "")
	_, err := cl.UploadFile(ctx, src, outside)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("want 403, got %v", err)
	}
}

func TestWatchJobMovesFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rootA := t.TempDir()
	rootB := t.TempDir()
	outbox := filepath.Join(rootA, "outbox")
	archive := filepath.Join(rootA, "sent")
	inbox := filepath.Join(rootB, "inbox")
	for _, d := range []string{outbox, archive, inbox} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	agentB, _ := startNode(t, ctx, nodeOpts{id: "b", token: longToken, root: rootB})
	agentA, _ := startNode(t, ctx, nodeOpts{
		id: "a", token: longToken, root: rootA,
		peers: []config.Peer{{
			ID: "b", URL: agentB.URL, Token: longToken, TLSSkipVerify: true,
		}},
		jobs: []config.Job{{
			Name: "watch-b", Watch: outbox, Pattern: "*.txt",
			Settle: config.D(200 * time.Millisecond), Interval: config.D(100 * time.Millisecond),
			Destination: config.Destination{Node: "b", Path: inbox},
			OnSuccess:   "move", ArchiveDir: archive,
		}},
	})
	_ = agentA
	payload := []byte("from watch")
	if err := os.WriteFile(filepath.Join(outbox, "auto.txt"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(inbox, "auto.txt")
	archived := filepath.Join(archive, "auto.txt")
	// Файл на получателе появляется раньше, чем отправитель успевает перенести исходник.
	if err := waitFor(t, 8*time.Second, func() bool {
		got, err := os.ReadFile(dest)
		if err != nil || !bytes.Equal(got, payload) {
			return false
		}
		if _, err := os.Stat(filepath.Join(outbox, "auto.txt")); !os.IsNotExist(err) {
			return false
		}
		_, err = os.Stat(archived)
		return err == nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rootA := t.TempDir()
	rootB := t.TempDir()
	srcDir := filepath.Join(rootA, "reports")
	inbox := filepath.Join(rootB, "inbox")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("nightly")
	if err := os.WriteFile(filepath.Join(srcDir, "r.txt"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	agentB, _ := startNode(t, ctx, nodeOpts{id: "b", token: longToken, root: rootB})
	_, _ = startNode(t, ctx, nodeOpts{
		id: "a", token: longToken, root: rootA,
		peers: []config.Peer{{
			ID: "b", URL: agentB.URL, Token: longToken, TLSSkipVerify: true,
		}},
		jobs: []config.Job{{
			Name: "every-sec", Schedule: "@every 1s",
			Source:      config.Source{Path: srcDir},
			Destination: config.Destination{Node: "b", Path: inbox + string(os.PathSeparator)},
			OnSuccess:   "none",
		}},
	})
	dest := filepath.Join(inbox, "r.txt")
	if err := waitFor(t, 12*time.Second, func() bool {
		got, err := os.ReadFile(dest)
		return err == nil && bytes.Equal(got, payload)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInboundAliasUpload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	agent, _ := startNode(t, ctx, nodeOpts{
		id: "b", token: longToken, root: root,
		inbound: []config.Inbound{{Name: "inbox", Path: inbox}},
	})
	src := filepath.Join(root, "local.txt")
	payload := []byte("via-alias")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	cl := mustClient(t, agent.URL, longToken, true, "")
	if _, err := cl.Upload(ctx, src, "nested/via.txt", "inbox"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(inbox, "nested", "via.txt"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("alias upload: %v %q", err, got)
	}
	if _, err := cl.Upload(ctx, src, `C:\ferry\x.txt`, "inbox"); err == nil {
		t.Fatal("absolute path with alias was accepted")
	}
}

func TestListDirectory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent, _ := startNode(t, ctx, nodeOpts{id: "a", token: longToken, root: root})
	cl := mustClient(t, agent.URL, longToken, true, "")
	list, err := cl.List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range list.Entries {
		if e.Name == "a.txt" && !e.IsDir && e.Size == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("entries %+v", list.Entries)
	}
}

const longToken = "test-token-0123456789"

type nodeOpts struct {
	id        string
	token     string
	root      string
	peers     []config.Peer
	jobs      []config.Job
	inbound   []config.Inbound
	scenarios []config.Scenario
}

func startNode(t *testing.T, ctx context.Context, opt nodeOpts) (*server.Agent, string) {
	t.Helper()
	certDir := filepath.Join(opt.root, "certs")
	res, err := cert.Generate(cert.Options{Dir: certDir, CommonName: opt.id})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Node: config.Node{
			ID:           opt.id,
			Listen:       "127.0.0.1:0",
			Token:        opt.token,
			DataDir:      filepath.Join(opt.root, "data"),
			AllowedRoots: []string{opt.root},
			TLS:          config.TLS{CertFile: res.CertFile, KeyFile: res.KeyFile},
		},
		Peers:     opt.peers,
		Jobs:      opt.jobs,
		Scenarios: opt.scenarios,
	}
	cfg.Node.Inbound = opt.inbound
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent, err := server.Start(ctx, cfg, log, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	return agent, res.CertFile
}

func mustClient(t *testing.T, rawURL, token string, skip bool, ca string) *client.Client {
	t.Helper()
	cl, err := client.New(config.Peer{
		ID: "peer", URL: rawURL, Token: token, TLSSkipVerify: skip, CAFile: ca,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

type failAfter struct {
	base   http.RoundTripper
	failOn int
	puts   int
}

func (f *failAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut {
		f.puts++
		if f.puts == f.failOn {
			return nil, errors.New("injected failure")
		}
	}
	return f.base.RoundTrip(r)
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) error {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timeout")
}
