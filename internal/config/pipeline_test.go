package config

import (
	"strings"
	"testing"
)

func TestPipeline_DefaultsToDirectAndAcceptsBothModes(t *testing.T) {
	if got := DefaultOzyd().Pipeline.Mode; got != PipelineDirect {
		t.Fatalf("default mode %q: it changes when the e2e and smoke suites run queued (M7 PR 7), not before", got)
	}
	for _, m := range []string{PipelineDirect, PipelineQueued} {
		p := Pipeline{Mode: m}
		if err := p.Validate(); err != nil {
			t.Errorf("mode %q rejected: %v", m, err)
		}
	}
}

func TestPipeline_RejectsAnUnknownModeAndANegativeCap(t *testing.T) {
	for _, p := range []Pipeline{{Mode: ""}, {Mode: "kafka"}, {Mode: PipelineQueued, Queue: Queue{MaxBytes: -1}}} {
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), "pipeline.") {
			t.Errorf("%+v: error %v, want one naming the field", p, err)
		}
	}
}

func TestPipeline_ModeCanBeSetFromTheEnvironment(t *testing.T) {
	cfg, _, err := LoadOzyd("", []string{"OZY_PIPELINE_MODE=queued", "OZY_PIPELINE_QUEUE_MAX_BYTES=1048576"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.Mode != PipelineQueued || cfg.Pipeline.Queue.MaxBytes != 1<<20 {
		t.Fatalf("%+v", cfg.Pipeline)
	}
}

func TestQuery_ValidateRejectsNegatives(t *testing.T) {
	for _, q := range []Query{{MaxConcurrent: -1}, {MaxQueue: -1}, {QueueWait: -1}} {
		if err := q.Validate(); err == nil {
			t.Errorf("%+v accepted", q)
		}
	}
	if err := (Query{}).Validate(); err != nil {
		t.Errorf("zero value (defaults) rejected: %v", err)
	}
}
