package prometheus_test

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	prometheus "github.com/Elagoht/collage-prometheus"
	"github.com/Elagoht/collage/pkg/collage"
	prom "github.com/prometheus/client_golang/prometheus"
)

// buildReader reads a finished build, as elagoht/deploy does; with one
// registered, the build asks the handler for every file to capture its headers.
type buildReader struct{ files []collage.BuiltFile }

func (*buildReader) Name() string                             { return "test/buildreader" }
func (*buildReader) Version() string                          { return "0" }
func (*buildReader) Init(context.Context, collage.Host) error { return nil }
func (*buildReader) Shutdown(context.Context) error           { return nil }
func (b *buildReader) OnBuildFinished(_ context.Context, ev *collage.BuildFinishedEvent) error {
	b.files = ev.Files
	return nil
}

// buildCounts builds a three-page site, cached, with the plugin as collage's
// Metrics, and returns every series it recorded: a histogram's sample count, a
// counter's value. With reader set, a buildReader is registered, so the build
// captures every file's headers.
func buildCounts(t *testing.T, reader *buildReader) map[string]float64 {
	t.Helper()
	reg := prom.NewRegistry()
	m := prometheus.NewMetrics(prometheus.Options{Registry: reg})
	plugins := []collage.Plugin{m}
	if reader != nil {
		plugins = append(plugins, reader)
	}
	app, err := collage.New(&collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html": {Data: []byte(`<main>home</main>`)},
		}, Root: "t"},
		Cache:         collage.CacheConfig{Enabled: true, Type: "memory", DefaultTTL: time.Hour},
		Observability: collage.ObservabilityConfig{Metrics: m},
		Plugins:       plugins,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/a", "/b"} {
		page := collage.NewPage("p"+path).WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", path).Build()
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	builder, err := collage.NewBuilder(app, collage.BuildOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := builder.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, f := range report.Findings {
		t.Errorf("finding: %s %s: %s", f.Rule, f.Path, f.Message)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]float64{}
	for _, mf := range families {
		for _, metric := range mf.GetMetric() {
			var labels []string
			for _, l := range metric.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			key := fmt.Sprintf("%s{%s}", mf.GetName(), strings.Join(labels, ","))
			switch {
			case metric.GetHistogram() != nil:
				counts[key] = float64(metric.GetHistogram().GetSampleCount())
			case metric.GetCounter() != nil:
				counts[key] = metric.GetCounter().GetValue()
			}
		}
	}
	return counts
}

// A static build's header capture is not counted: the build records the same
// series with its capture as without it — no HTTP response, no render, no
// fragment, no cache event of its own.
func TestBuildCaptureIsNotCounted(t *testing.T) {
	without := buildCounts(t, nil)
	reader := &buildReader{}
	with := buildCounts(t, reader)
	if !maps.Equal(with, without) {
		keys := make([]string, 0, len(with))
		for k := range with {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if with[k] != without[k] {
				t.Errorf("%s: %v with the capture, %v without it", k, with[k], without[k])
			}
		}
		for k, v := range without {
			if _, ok := with[k]; !ok {
				t.Errorf("%s: missing with the capture, %v without it", k, v)
			}
		}
	}
	for k := range with {
		if strings.HasPrefix(k, "collage_http_request_duration_seconds") {
			t.Errorf("%s recorded: the build serves no client", k)
		}
	}
	captured := 0
	for _, f := range reader.files {
		if !f.Captured {
			continue
		}
		captured++
		if f.Status != http.StatusOK {
			t.Errorf("%s: status %d", f.Path, f.Status)
		}
	}
	if captured != 3 {
		t.Fatalf("%d pages captured, want 3", captured)
	}
}
