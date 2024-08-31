// Copyright 2022 The Cloudprober Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package sched provides utilities to schedule Probes.
package sched

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/cloudprober/cloudprober/metrics"
	"github.com/cloudprober/cloudprober/probes/options"
	"github.com/cloudprober/cloudprober/targets/endpoint"
)

// DefaultTargetsUpdateInterval defines default frequency for target updates.
// Actual targets update interval is:
// max(DefaultTargetsUpdateInterval, probe_interval)
var DefaultTargetsUpdateInterval = 1 * time.Minute

func CtxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// ProbeResult represents results of a probe run.
type ProbeResult interface {
	// Metrics returns ProbeResult metrics as a metrics.EventMetrics object.
	// This EventMetrics object should not be reused for further accounting
	// because it's modified by the scheduler and later on when it's pushed
	// to the data channel.
	Metrics(timeStamp time.Time, runID int64, opts *options.Options) *metrics.EventMetrics
}

type Scheduler struct {
	ProbeName string
	DataChan  chan *metrics.EventMetrics
	Opts      *options.Options

	// NewResult may or may not make use of the provided target. It's useful
	// you want to handle result objects yourself, for example to share them
	// across targets. See grpc probe for an example.
	NewResult func(target *endpoint.Endpoint) ProbeResult

	// ListEndpoints is optional. If not provided, we just call the default
	// lister: opts.Tagets.ListEndpoints(). See grpc probe for an example of
	// how to use this field.
	ListEndpoints func() []endpoint.Endpoint

	StartForTarget func(ctx context.Context, target endpoint.Endpoint)

	// RunProbeForTarget is called per probe cycle for each target.
	RunProbeForTarget func(context.Context, endpoint.Endpoint, ProbeResult)

	// IntervalBetweenTargets defines the time between target updates.
	IntervalBetweenTargets time.Duration

	statsExportFrequency  int64
	targetsUpdateInterval time.Duration
	targets               []endpoint.Endpoint
	waitGroup             sync.WaitGroup
	cancelFuncs           map[string]context.CancelFunc
}

func (s *Scheduler) init() {
	if s.cancelFuncs == nil {
		s.cancelFuncs = make(map[string]context.CancelFunc)
	}

	s.statsExportFrequency = s.Opts.StatsExportInterval.Nanoseconds() / s.Opts.Interval.Nanoseconds()
	if s.statsExportFrequency == 0 {
		s.statsExportFrequency = 1
	}

	s.targetsUpdateInterval = DefaultTargetsUpdateInterval
	// There is no point refreshing targets before probe interval.
	if s.targetsUpdateInterval < s.Opts.Interval {
		s.targetsUpdateInterval = s.Opts.Interval
	}
	s.Opts.Logger.Infof("Targets update interval: %v", s.targetsUpdateInterval)
}

func (s *Scheduler) gapBetweenTargets() time.Duration {
	interTargetGap := s.IntervalBetweenTargets

	// If not configured by user, determine based on probe interval and number of
	// targets.
	if interTargetGap == 0 && len(s.targets) != 0 {
		// Use 1/10th of the probe interval to spread out target groroutines.
		interTargetGap = s.Opts.Interval / time.Duration(10*len(s.targets))
	}

	return interTargetGap
}

func (s *Scheduler) startForTarget(ctx context.Context, target endpoint.Endpoint) {
	s.Opts.Logger.Debug("Starting probing for the target ", target.Name)

	if s.StartForTarget != nil {
		s.StartForTarget(ctx, target)
		return
	}

	// We use this counter to decide when to export stats.
	var runCnt int64

	result := s.NewResult(&target)

	ticker := time.NewTicker(s.Opts.Interval)
	defer ticker.Stop()

	for ts := time.Now(); true; ts = <-ticker.C {
		// Don't run another probe if context is canceled already.
		if CtxDone(ctx) {
			return
		}
		if !s.Opts.IsScheduled() {
			continue
		}
		s.RunProbeForTarget(ctx, target, result)

		// Export stats if it's the time to do so.
		runCnt++
		if (runCnt % s.statsExportFrequency) == 0 {
			em := result.Metrics(ts, runCnt, s.Opts)
			// Returning nil is a way to skip this target. Used by grpc probe.
			if em == nil {
				continue
			}
			em.AddLabel("probe", s.ProbeName).
				AddLabel("dst", target.Dst())
			s.Opts.RecordMetrics(target, em, s.DataChan)
		}
	}
}

func (s *Scheduler) Wait() {
	s.waitGroup.Wait()
}

// refreshTargets refreshes targets and starts probe loop for
// new targets and cancels probe loops for targets that are no longer active.
// Note that this function is not concurrency safe. It is never called
// concurrently by Start().
func (s *Scheduler) refreshTargets(ctx context.Context) {
	var newTargets []endpoint.Endpoint
	if s.ListEndpoints != nil {
		newTargets = s.ListEndpoints()
	} else {
		newTargets = s.Opts.Targets.ListEndpoints()
	}

	s.Opts.Logger.Debugf("Probe(%s) got %d targets", s.ProbeName, len(s.targets))

	s.targets = newTargets

	// updatedTargets is used only for logging.
	updatedTargets := make(map[string]string)
	defer func() {
		if len(updatedTargets) > 0 {
			s.Opts.Logger.Infof("Probe(%s) targets updated: %v", s.ProbeName, updatedTargets)
		}
	}()

	activeTargets := make(map[string]endpoint.Endpoint)
	for _, target := range s.targets {
		key := target.Key()
		activeTargets[key] = target
	}

	// Stop probing for deleted targets by invoking cancelFunc.
	for targetKey, cancelF := range s.cancelFuncs {
		if _, ok := activeTargets[targetKey]; ok {
			continue
		}
		cancelF()
		updatedTargets[targetKey] = "DELETE"
		delete(s.cancelFuncs, targetKey)
	}

	gapBetweenTargets := s.gapBetweenTargets()
	var startWaitTime time.Duration

	// Start probe loop for new targets.
	for key, target := range activeTargets {
		// This target is already initialized.
		if _, ok := s.cancelFuncs[key]; ok {
			continue
		}
		updatedTargets[key] = "ADD"

		probeCtx, cancelF := context.WithCancel(ctx)
		s.waitGroup.Add(1)

		go func(target endpoint.Endpoint, waitTime time.Duration) {
			defer s.waitGroup.Done()
			if waitTime > 0 {
				// For random padding using 1/10th of the gap.
				jitterMaxUsec := gapBetweenTargets.Microseconds() / 10
				// Make sure we don't pass 0 to rand.Int63n.
				if jitterMaxUsec <= 0 {
					jitterMaxUsec = 1
				}
				// Wait for wait time + some jitter before starting this probe loop.
				time.Sleep(waitTime + time.Duration(rand.Int63n(jitterMaxUsec))*time.Microsecond)
			}
			s.startForTarget(probeCtx, target)
		}(target, startWaitTime)

		startWaitTime += gapBetweenTargets

		s.cancelFuncs[key] = cancelF
	}
}

func (s *Scheduler) UpdateTargetsAndStartProbes(ctx context.Context) {
	defer s.Wait()

	// Initialize scheduler.
	s.init()

	s.refreshTargets(ctx)

	// Do more frequent listing of targets until we get a non-zero list of
	// targets.
	initialRefreshInterval := s.Opts.Interval
	// Don't wait too long if s.Opts.Interval is large.
	if initialRefreshInterval > time.Second {
		initialRefreshInterval = time.Second
	}

	for {
		if CtxDone(ctx) {
			return
		}
		if len(s.targets) != 0 {
			break
		}
		s.refreshTargets(ctx)
		time.Sleep(initialRefreshInterval)
	}

	targetsUpdateTicker := time.NewTicker(s.targetsUpdateInterval)
	defer targetsUpdateTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-targetsUpdateTicker.C:
			s.refreshTargets(ctx)
		}
	}
}
