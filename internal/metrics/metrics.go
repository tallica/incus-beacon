// Package metrics serves counters, gauges and histograms in the Prometheus text format.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Set holds metrics in the order they were made, which is the order served.
type Set struct {
	mu      sync.Mutex
	metrics []metric
}

type metric interface {
	write(w io.Writer)
}

func (s *Set) add(m metric) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = append(s.metrics, m)
}

func (s *Set) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.mu.Lock()
		ms := slices.Clone(s.metrics)
		s.mu.Unlock()
		bw := bufio.NewWriter(w)
		for _, m := range ms {
			m.write(bw)
		}
		_ = bw.Flush()
	})
}

type desc struct {
	name, help, kind string
	labels           []string
}

func (d *desc) header(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", d.name, d.help, d.name, d.kind)
}

// key panics on a wrong label count: that is a bug at the call site.
func (d *desc) key(values []string) string {
	if len(values) != len(d.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", d.name, len(d.labels), len(values)))
	}
	return strings.Join(values, "\xff")
}

func (d *desc) labelPairs(values []string, extra ...string) string {
	pairs := make([]string, 0, len(values)+1)
	for i, v := range values {
		pairs = append(pairs, d.labels[i]+`="`+escape(v)+`"`)
	}
	pairs = append(pairs, extra...)
	if len(pairs) == 0 {
		return ""
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(v string) string { return escaper.Replace(v) }

func format(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

type point struct {
	labels []string
	value  float64
}

type values struct {
	desc
	mu     sync.Mutex
	points map[string]*point
}

func (v *values) init(name, help, kind string, labels []string) {
	v.desc = desc{name: name, help: help, kind: kind, labels: labels}
	v.points = map[string]*point{}
	if len(labels) == 0 {
		v.points[""] = &point{}
	}
}

func (v *values) update(labels []string, f func(*float64)) {
	k := v.key(labels)
	v.mu.Lock()
	defer v.mu.Unlock()
	p, ok := v.points[k]
	if !ok {
		p = &point{labels: slices.Clone(labels)}
		v.points[k] = p
	}
	f(&p.value)
}

func (v *values) write(w io.Writer) {
	v.header(w)
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, k := range slices.Sorted(maps.Keys(v.points)) {
		p := v.points[k]
		fmt.Fprintf(w, "%s%s %s\n", v.name, v.labelPairs(p.labels), format(p.value))
	}
}

type Counter struct{ values }

func (s *Set) Counter(name, help string, labels ...string) *Counter {
	c := &Counter{}
	c.init(name, help, "counter", labels)
	s.add(c)
	return c
}

func (c *Counter) Add(d float64, labels ...string) {
	c.update(labels, func(v *float64) { *v += d })
}

type Gauge struct{ values }

func (s *Set) Gauge(name, help string, labels ...string) *Gauge {
	g := &Gauge{}
	g.init(name, help, "gauge", labels)
	s.add(g)
	return g
}

func (g *Gauge) Set(x float64, labels ...string) {
	g.update(labels, func(v *float64) { *v = x })
}

type Histogram struct {
	desc
	bounds []float64
	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	labels []string
	counts []uint64 // per bound, not cumulative
	sum    float64
	count  uint64
}

// Histogram's bounds are the upper bounds of its buckets, in ascending order.
func (s *Set) Histogram(name, help string, bounds []float64, labels ...string) *Histogram {
	h := &Histogram{
		desc:   desc{name: name, help: help, kind: "histogram", labels: labels},
		bounds: bounds,
		series: map[string]*series{},
	}
	s.add(h)
	return h
}

func (h *Histogram) Observe(x float64, labels ...string) {
	k := h.key(labels)
	h.mu.Lock()
	defer h.mu.Unlock()
	sr, ok := h.series[k]
	if !ok {
		sr = &series{labels: slices.Clone(labels), counts: make([]uint64, len(h.bounds))}
		h.series[k] = sr
	}
	if i, _ := slices.BinarySearch(h.bounds, x); i < len(h.bounds) {
		sr.counts[i]++
	}
	sr.sum += x
	sr.count++
}

func (h *Histogram) write(w io.Writer) {
	h.header(w)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range slices.Sorted(maps.Keys(h.series)) {
		sr := h.series[k]
		var cum uint64
		for i, b := range h.bounds {
			cum += sr.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, h.labelPairs(sr.labels, `le="`+format(b)+`"`), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, h.labelPairs(sr.labels, `le="+Inf"`), sr.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, h.labelPairs(sr.labels), format(sr.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, h.labelPairs(sr.labels), sr.count)
	}
}
