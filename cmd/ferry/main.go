// Команда ferry — агент передачи файлов и CLI к нему.
// Версия подставляется при сборке: -X main.version=...
package main

import (
	"os"

	"github.com/UncleUSN/my-repo/internal/cli"
)

var version = "0.1.0"

func main() {
	os.Exit(cli.Main(version, os.Args[1:]))
}
