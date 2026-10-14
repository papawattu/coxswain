// loops_active_collector.go — B5a: custom Prometheus collector for
// coxswain_loops_active. Instead of a GaugeVec set on reconcile (which leaves
// stale labels for phases a Loop left), this collector counts Loops per
// non-terminal phase from the manager's cache at scrape time. This gives an
// accurate snapshot of how many Loops are currently in each active phase.

package controller

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// loopsActivePhaseLabel is the label name for the coxswain_loops_active metric.
// goconst: a single const avoids the goconst lint on the repeated "phase" string.
const loopsActivePhaseLabel = "phase"

// loopsActiveCollector is a custom Prometheus collector that counts Loops per
// non-terminal phase from the cache at scrape time. It replaces the
// GaugeVec (which set labels on reconcile and left stale values for phases
// a Loop had already left) with an accurate count of the current state.
//
// The collector needs a client to List Loops from the cache. It is wired up
// in the SetupWithManager path (where the manager's cache is available).
// In envtest / bare unit tests, the collector is not registered (the
// GaugeVec is kept for backward compatibility but is no longer the source
// of truth).
type loopsActiveCollector struct {
	client client.Client
}

// NewLoopsActiveCollector creates a new loopsActiveCollector with the given
// client (typically the manager's cached client).
func NewLoopsActiveCollector(c client.Client) prometheus.Collector {
	return &loopsActiveCollector{client: c}
}

// Describe implements prometheus.Collector. It returns the metric's descriptor.
func (c *loopsActiveCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc(
		"coxswain_loops_active",
		"Loops currently in a non-terminal phase, by phase (counted from the cache at scrape time).",
		[]string{loopsActivePhaseLabel},
		nil,
	)
}

// Collect implements prometheus.Collector. It Lists Loops from the cache and
// counts how many are in each non-terminal phase.
func (c *loopsActiveCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	loops := &coxv1alpha1.LoopList{}
	// List all Loops in the cluster (the cache is scoped to the manager's
	// watches, so this is a fast in-memory operation).
	if err := c.client.List(ctx, loops); err != nil {
		// If the List fails (e.g. CRD not installed, cache not ready),
		// emit no metrics. The scrape will show no coxswain_loops_active
		// lines, which is a safe default.
		return
	}

	// Count Loops per non-terminal phase.
	phaseCounts := make(map[string]int)
	for _, loop := range loops.Items {
		phase := string(loop.Status.Phase)
		if phase == "" || isTerminalPhase(loop.Status.Phase) {
			continue
		}
		// Skip Loops being deleted (finalizer processing).
		if loop.DeletionTimestamp != nil {
			continue
		}
		phaseCounts[phase]++
	}

	// Emit a metric for each phase that has at least one active Loop.
	for phase, count := range phaseCounts {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				"coxswain_loops_active",
				"Loops currently in a non-terminal phase, by phase (counted from the cache at scrape time).",
				[]string{loopsActivePhaseLabel},
				nil,
			),
			prometheus.GaugeValue,
			float64(count),
			phase,
		)
	}
}

// isTerminalPhase is already defined in loop_controller.go (B5a).
// We reuse it here.
