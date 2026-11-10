package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/intake"
	"github.com/tuvo1106/ozymandias/internal/queue"
)

// queueDir holds one topic directory per signal under data_dir.
const queueDir = "queue"

// pipeline is the queue half of queued mode: a topic per signal, and the
// consumer that feeds each topic's store.
type pipeline struct {
	topics    map[string]*queue.Topic
	consumers map[string]*queue.Consumer
	// pending is, per topic, the produce time (unix nanos) of the oldest record
	// the consumer holds but has not committed; zero when it holds none.
	pending map[string]*atomic.Int64
}

// openPipeline opens every topic and registers each store's consumer. The
// consumers are registered here, before anything is served, so retention is
// held back by them from the first record rather than from whenever Run gets
// going.
func (s *Server) openPipeline() (*pipeline, error) {
	p := &pipeline{
		topics:    map[string]*queue.Topic{},
		consumers: map[string]*queue.Consumer{},
		pending:   map[string]*atomic.Int64{},
	}
	for _, name := range intake.Topics {
		t, err := queue.Open(queue.Options{
			Dir:          filepath.Join(s.cfg.DataDir, queueDir, name),
			Clock:        s.clock,
			MaxBytes:     s.cfg.Pipeline.Queue.MaxBytes,
			SyncInterval: s.cfg.Pipeline.Queue.SyncInterval,
		})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("queue %s: %w", name, err), p.close())
		}
		p.topics[name] = t
		c, err := t.Consumer(intake.ConsumerName(name))
		if err != nil {
			return nil, errors.Join(fmt.Errorf("queue %s: %w", name, err), p.close())
		}
		p.consumers[name] = c
		p.pending[name] = new(atomic.Int64)
	}
	return p, nil
}

// producers is the intake's view of the topics.
func (p *pipeline) producers() map[string]intake.Producer {
	out := make(map[string]intake.Producer, len(p.topics))
	for name, t := range p.topics {
		out[name] = t
	}
	return out
}

func (p *pipeline) close() error {
	var errs []error
	for _, c := range p.consumers {
		c.Close()
	}
	for _, t := range p.topics {
		errs = append(errs, t.Close())
	}
	return errors.Join(errs...)
}

// registerQueueMetrics exposes depth and lag per topic. Lag is what an
// operator watches: records says how much work is waiting, seconds says how
// stale the stores are.
func (s *Server) registerQueueMetrics() {
	for name, t := range s.pipe.topics {
		tag := "topic:" + name
		consumer := intake.ConsumerName(name)
		pending := s.pipe.pending[name]
		s.reg.GaugeFunc("ozy.queue.lag_records", func() float64 { return float64(t.Stats().Lag[consumer]) }, tag)
		s.reg.GaugeFunc("ozy.queue.lag_seconds", func() float64 {
			// A caught-up consumer has no pending record; reading zero then is
			// the fact, not a missing value.
			at := pending.Load()
			if at == 0 || t.Stats().Lag[consumer] == 0 {
				return 0
			}
			return max(0, s.clock.Now().Sub(time.Unix(0, at)).Seconds())
		}, tag)
		s.reg.GaugeFunc("ozy.queue.bytes", func() float64 { return float64(t.Stats().Bytes) }, tag)
		s.reg.GaugeFunc("ozy.queue.broken", func() float64 {
			if t.Stats().Broken != nil {
				return 1
			}
			return 0
		}, tag)
	}
}

// runConsumers starts one loop per topic and returns how many it started.
func (s *Server) runConsumers(ctx context.Context, done chan<- struct{}) int {
	for _, name := range intake.Topics {
		go func() {
			err := s.intake.RunConsumer(ctx, name, s.pipe.consumers[name], s.pipe.pending[name])
			if err != nil {
				s.log.Error("consumer stopped; the "+name+" store is no longer being fed", "topic", name, "err", err)
			}
			done <- struct{}{}
		}()
	}
	return len(intake.Topics)
}
