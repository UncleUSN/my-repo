package jobs

import (
	"github.com/UncleUSN/my-repo/internal/config"
	"github.com/UncleUSN/my-repo/internal/paths"
)

// destinationPath собирает путь на узле-получателе.
// Каталог (watch, источник-каталог или путь с хвостовым слэшем) + относительное имя.
// Один файл без хвостового слэша кладётся ровно в destination.path.
// Стиль разделителей берётся из пути назначения, а не из ОС отправителя:
// агент в WSL может собрать C:\ferry\inbox\a.txt для узла на Windows.
func destinationPath(job config.Job, srcIsDir bool, rel string) (string, error) {
	dirMode := srcIsDir || job.Watch != "" || paths.HasTrailingSeparator(job.Destination.Path)
	if !dirMode {
		return paths.Clean(job.Destination.Path)
	}
	return paths.Join(job.Destination.Path, rel)
}
