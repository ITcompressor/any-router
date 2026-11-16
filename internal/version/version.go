// Package version — версия any-router и метаданные сборки (механика zeph/ralph):
// переменные пакета, Commit/BuildTime вкачиваются через -ldflags при сборке
// (any_router_ldflags), дефолты означают «собрано без метаданных»
// (go build/run напрямую). Печатается `any-router version`, отдаётся в
// GET /api/version и показывается в топбаре админки.
package version

import "fmt"

var (
	// Version — версия any-router. Переменная (не константа):
	// переопределяется -ldflags при сборке; повышать при каждом изменении,
	// затрагивающем CLI.
	Version = "v0.2.0"
	// Commit и BuildTime — метаданные сборки (-ldflags при сборке).
	Commit    = "dev"
	BuildTime = "unknown"
)

// String — «Version (commit, time)»; без метаданных — просто Version.
func String() string {
	if Commit == "dev" && BuildTime == "unknown" {
		return Version
	}
	return fmt.Sprintf("%s (%s, %s)", Version, Commit, BuildTime)
}

// Short — версия + commit для веб-морды (время сборки не показываем).
func Short() string {
	if Commit == "dev" {
		return Version
	}
	return Version + " (" + Commit + ")"
}