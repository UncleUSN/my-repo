// Package cli разбирает команды ferry.
// Тексты для человека на русском: ими пользуется и терминал, и журнал ошибок CI.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/UncleUSN/my-repo/internal/cert"
	"github.com/UncleUSN/my-repo/internal/client"
	"github.com/UncleUSN/my-repo/internal/config"
)

const helpText = `ferry — передача файлов между Windows и Linux.

Использование:
  ferry serve --config ferry.yaml
  ferry send --config ferry.yaml --peer <узел> --src <локальный файл> --dst <путь на узле>
  ferry get  --config ferry.yaml --peer <узел> --src <путь на узле> --dst <локальный файл>
  ferry ls   --config ferry.yaml --peer <узел> --path <каталог>
  ferry ping --config ferry.yaml --peer <узел>
  ferry jobs list --config ferry.yaml
  ferry jobs run --config ferry.yaml <имя>
  ferry transfers --config ferry.yaml
  ferry cert --dir <каталог> [--host имя]... [--ip адрес]...
  ferry service install|uninstall|start|stop|restart --config ferry.yaml
  ferry version

send и get ходят на другой узел напрямую и не требуют локального демона.
jobs, transfers и service управляют агентом, который уже запущен через serve.

Флаги указывайте до позиционных аргументов.
Переменная FERRY_CONFIG задаёт путь к конфигу по умолчанию.
В YAML можно подставлять переменные окружения: "https://${WIN_HOST}:7420".
`

// Main выполняет одну команду и возвращает код выхода.
func Main(version string, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stdout, helpText)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println(version)
		return 0
	case "serve":
		return cmdServe(version, args[1:])
	case "send":
		return cmdSend(args[1:])
	case "get":
		return cmdGet(args[1:])
	case "ls":
		return cmdLs(args[1:])
	case "ping":
		return cmdPing(args[1:])
	case "jobs":
		return cmdJobs(args[1:])
	case "transfers":
		return cmdTransfers(args[1:])
	case "cert":
		return cmdCert(args[1:])
	case "service":
		return cmdService(version, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n%s", args[0], helpText)
		return 2
	}
}

func cmdServe(version string, args []string) int {
	fs := newFlagSet("serve")
	cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if err := runService(version, *cfgPath, ""); err != nil {
		return fail(err)
	}
	return 0
}

func cmdService(version string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "укажите действие: install, uninstall, start, stop или restart\n")
		return 2
	}
	action := args[0]
	switch action {
	case "install", "uninstall", "start", "stop", "restart":
	default:
		fmt.Fprintf(os.Stderr, "неизвестное действие службы %q\n", action)
		return 2
	}
	fs := newFlagSet("service " + action)
	cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
	if code := parse(fs, args[1:]); code >= 0 {
		return code
	}
	if err := runService(version, *cfgPath, action); err != nil {
		return fail(err)
	}
	fmt.Printf("служба: %s выполнено\n", action)
	return 0
}

func cmdSend(args []string) int {
	fs := newFlagSet("send")
	cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
	peerID := fs.String("peer", "", "идентификатор узла из peers")
	src := fs.String("src", "", "локальный файл")
	dst := fs.String("dst", "", "путь на удалённом узле")
	token := fs.String("token", "", "переопределить токен узла")
	jsonOut := fs.Bool("json", false, "напечатать JSON")
	chunk := fs.Int("chunk", client.DefaultChunkSize, "размер части в байтах")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if *peerID == "" || *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "нужны флаги --peer, --src и --dst")
		return 2
	}
	cl, err := peerClient(*cfgPath, *peerID, *token, *chunk)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	res, err := cl.UploadFile(ctx, *src, *dst)
	if err != nil {
		return fail(err)
	}
	printTransfer("отправлено", *peerID, *src, *dst, res, *jsonOut)
	return 0
}

func cmdGet(args []string) int {
	fs := newFlagSet("get")
	cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
	peerID := fs.String("peer", "", "идентификатор узла из peers")
	src := fs.String("src", "", "путь на удалённом узле")
	dst := fs.String("dst", "", "локальный файл")
	token := fs.String("token", "", "переопределить токен узла")
	jsonOut := fs.Bool("json", false, "напечатать JSON")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if *peerID == "" || *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "нужны флаги --peer, --src и --dst")
		return 2
	}
	cl, err := peerClient(*cfgPath, *peerID, *token, 0)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	res, err := cl.DownloadFile(ctx, *src, *dst)
	if err != nil {
		return fail(err)
	}
	printTransfer("получено", *peerID, *src, *dst, res, *jsonOut)
	return 0
}

func cmdLs(args []string) int {
	fs := newFlagSet("ls")
	cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
	peerID := fs.String("peer", "", "идентификатор узла из peers")
	path := fs.String("path", "", "каталог на узле")
	token := fs.String("token", "", "переопределить токен узла")
	jsonOut := fs.Bool("json", false, "напечатать JSON")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if *peerID == "" || *path == "" {
		fmt.Fprintln(os.Stderr, "нужны флаги --peer и --path")
		return 2
	}
	cl, err := peerClient(*cfgPath, *peerID, *token, 0)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	list, err := cl.List(ctx, *path)
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(list)
		return 0
	}
	if len(list.Entries) == 0 {
		fmt.Printf("%s: пусто\n", list.Path)
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ТИП\tРАЗМЕР\tИМЯ")
	for _, e := range list.Entries {
		kind := "файл"
		if e.IsDir {
			kind = "каталог"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", kind, e.Size, e.Name)
	}
	_ = tw.Flush()
	return 0
}

func cmdPing(args []string) int {
	fs := newFlagSet("ping")
	cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
	peerID := fs.String("peer", "", "идентификатор узла из peers")
	token := fs.String("token", "", "переопределить токен узла")
	jsonOut := fs.Bool("json", false, "напечатать JSON")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if *peerID == "" {
		fmt.Fprintln(os.Stderr, "нужен флаг --peer")
		return 2
	}
	cl, err := peerClient(*cfgPath, *peerID, *token, 0)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	id, err := cl.WhoAmI(ctx)
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		fmt.Printf("{\"peer\":%q,\"node\":%q}\n", *peerID, id)
		return 0
	}
	fmt.Printf("узел %s отвечает (id=%s)\n", *peerID, id)
	return 0
}

func cmdJobs(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "укажите list или run")
		return 2
	}
	switch args[0] {
	case "list":
		fs := newFlagSet("jobs list")
		cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
		jsonOut := fs.Bool("json", false, "напечатать JSON")
		if code := parse(fs, args[1:]); code >= 0 {
			return code
		}
		cl, err := localClient(*cfgPath)
		if err != nil {
			return fail(err)
		}
		ctx, cancel := signalContext()
		defer cancel()
		jobs, err := cl.Jobs(ctx)
		if err != nil {
			return fail(err)
		}
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(jobs)
			return 0
		}
		if len(jobs) == 0 {
			fmt.Println("заданий нет")
			return 0
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ИМЯ\tТИП\tСОСТОЯНИЕ\tПОСЛЕДНИЙ ЗАПУСК")
		for _, j := range jobs {
			state := "ожидает"
			if j.Running {
				state = "выполняется"
			} else if j.LastStatus != "" {
				state = j.LastStatus
			}
			last := "-"
			if j.LastRun != nil {
				last = j.LastRun.Local().Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", j.Name, j.Kind, state, last)
		}
		_ = tw.Flush()
		return 0
	case "run":
		fs := newFlagSet("jobs run")
		cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
		jsonOut := fs.Bool("json", false, "напечатать JSON")
		if code := parse(fs, args[1:]); code >= 0 {
			return code
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "использование: ferry jobs run --config <файл> <имя>")
			return 2
		}
		cl, err := localClient(*cfgPath)
		if err != nil {
			return fail(err)
		}
		ctx, cancel := signalContext()
		defer cancel()
		res, err := cl.RunJob(ctx, fs.Arg(0))
		if *jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(res)
		}
		if err != nil {
			if !*jsonOut {
				fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
			}
			return 1
		}
		if !*jsonOut {
			fmt.Printf("задание %s: передано %d, ошибок %d\n", res.Name, res.Transferred, res.Failed)
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда jobs %q\n", args[0])
		return 2
	}
}

func cmdTransfers(args []string) int {
	fs := newFlagSet("transfers")
	cfgPath := fs.String("config", defaultConfig(), "путь к YAML-конфигу")
	limit := fs.Int("limit", 20, "сколько записей показать")
	jsonOut := fs.Bool("json", false, "напечатать JSON")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	cl, err := localClient(*cfgPath)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	list, err := cl.Transfers(ctx, *limit)
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(list)
		return 0
	}
	if len(list) == 0 {
		fmt.Println("история пуста")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ВРЕМЯ\tСТАТУС\tНАПРАВЛЕНИЕ\tБАЙТ\tПУТЬ")
	for _, t := range list {
		when := t.StartedAt.Local().Format("2006-01-02 15:04:05")
		p := t.LocalPath
		if t.RemotePath != "" {
			p = t.LocalPath + " -> " + t.RemotePath
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", when, t.Status, t.Direction, t.Size, p)
	}
	_ = tw.Flush()
	return 0
}

func cmdCert(args []string) int {
	fs := newFlagSet("cert")
	dir := fs.String("dir", "certs", "каталог для cert.pem и key.pem")
	days := fs.Int("days", 825, "срок действия, дней")
	cn := fs.String("cn", "ferry", "Common Name")
	var hosts stringList
	var ips stringList
	fs.Var(&hosts, "host", "DNS-имя в сертификате (флаг можно повторять)")
	fs.Var(&ips, "ip", "IP-адрес в сертификате (флаг можно повторять)")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	res, err := cert.Generate(cert.Options{
		Dir: *dir, Hosts: hosts, IPs: ips, Days: *days, CommonName: *cn,
	})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("сертификат: %s\nключ: %s\n", res.CertFile, res.KeyFile)
	fmt.Println("localhost и 127.0.0.1 добавлены автоматически.")
	return 0
}

func peerClient(cfgPath, peerID, token string, chunk int) (*client.Client, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	peer, ok := cfg.Peer(peerID)
	if !ok {
		return nil, fmt.Errorf("в конфиге нет узла %q", peerID)
	}
	if token != "" {
		peer.Token = token
	}
	cl, err := client.New(peer)
	if err != nil {
		return nil, err
	}
	if chunk > 0 {
		cl.SetChunkSize(chunk)
	}
	return cl, nil
}

func localClient(cfgPath string) (*client.Client, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if err := cfg.PrepareRuntime(); err != nil {
		return nil, err
	}
	return client.DialLocal(cfg)
}

func printTransfer(action, peer, src, dst string, res client.Result, jsonOut bool) {
	note := ""
	if res.AlreadyExists {
		note = "already_exists"
	} else if res.Resumed {
		note = "resumed"
	}
	if jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"status":         "ok",
			"action":         action,
			"peer":           peer,
			"src":            src,
			"dst":            dst,
			"size":           res.Size,
			"sha256":         res.SHA256,
			"resumed":        res.Resumed,
			"already_exists": res.AlreadyExists,
			"path":           res.Path,
		})
		return
	}
	extra := ""
	switch note {
	case "already_exists":
		extra = ", уже был на месте"
	case "resumed":
		extra = ", с докачкой"
	}
	fmt.Printf("%s: %s -> %s:%s (%d байт, sha256 %s%s)\n", action, src, peer, dst, res.Size, res.SHA256, extra)
}

func defaultConfig() string {
	if p := os.Getenv("FERRY_CONFIG"); p != "" {
		return p
	}
	return "ferry.yaml"
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "ferry %s\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// parse возвращает -1, если флаги разобраны. 0 — показана справка, 2 — ошибка.
func parse(fs *flag.FlagSet, args []string) int {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return -1
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
	return 1
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

type stringList []string

func (s *stringList) String() string { return fmt.Sprint([]string(*s)) }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
