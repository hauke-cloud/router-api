// Package version carries the build metadata stamped in at link time.
package version

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
)

// Set through -ldflags -X at build time; see the Makefile and Dockerfile.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Version returns the release version.
func Version() string { return version }

// Commit returns the git revision the binary was built from.
func Commit() string { return commit }

// Date returns the build timestamp.
func Date() string { return date }

// Collector returns a constant metric describing the running build, the
// conventional way to make a version queryable in Prometheus.
func Collector() prometheus.Collector {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "router_api",
		Name:      "build_info",
		Help:      "Build metadata of the running manager, always 1.",
	}, []string{"version", "commit", "date", "goversion"})
	g.WithLabelValues(version, commit, date, runtime.Version()).Set(1)
	return g
}
