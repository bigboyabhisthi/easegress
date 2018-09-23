package engine

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexdecteam/easegateway/pkg/common"
	"github.com/hexdecteam/easegateway/pkg/logger"
	"github.com/hexdecteam/easegateway/pkg/model"
	"github.com/hexdecteam/easegateway/pkg/option"
	pipelines_gw "github.com/hexdecteam/easegateway/pkg/pipelines"

	"github.com/hexdecteam/easegateway-types/pipelines"
)

type pipelineInstance struct {
	instance pipelines_gw.Pipeline
	stop     chan struct{}
	stopped  chan struct{}
	done     chan struct{}
}

func newPipelineInstance(instance pipelines_gw.Pipeline) *pipelineInstance {
	return &pipelineInstance{
		instance: instance,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (pi *pipelineInstance) prepare() {
	pi.instance.Prepare()
}

func (pi *pipelineInstance) run() {
loop:
	for {
		select {
		case <-pi.stop:
			break loop
		default:
			err := pi.instance.Run()
			if err != nil {
				logger.Errorf(
					"[pipeline %s runs error and exits exceptionally: %v]",
					pi.instance.Name(), err)
				break loop
			}
		}
	}

	<-pi.stopped
	pi.instance.Close()
	close(pi.done)
}

func (pi *pipelineInstance) terminate(scheduled bool) chan struct{} {
	close(pi.stop)
	go func() { // Stop() blocks until Run() exits
		pi.instance.Stop(scheduled)
		close(pi.stopped)
	}()
	return pi.done
}

////

type PipelineScheduler interface {
	PipelineName() string
	SourceInputTrigger() pipelines.SourceInputTrigger
	Start(ctx pipelines.PipelineContext, statistics *model.PipelineStatistics, mod *model.Model)
	Stop()
	StopPipeline()
}

////

const PIPELINE_STOP_TIMEOUT_SECONDS = 30

type commonPipelineScheduler struct {
	pipeline         *model.Pipeline
	instancesLock    sync.RWMutex
	instances        []*pipelineInstance
	started, stopped uint32
}

func newCommonPipelineScheduler(pipeline *model.Pipeline) *commonPipelineScheduler {
	return &commonPipelineScheduler{
		pipeline: pipeline,
	}
}

func (scheduler *commonPipelineScheduler) PipelineName() string {
	return scheduler.pipeline.Name()
}

func (scheduler *commonPipelineScheduler) startPipeline(parallelism uint32,
	ctx pipelines.PipelineContext, statistics *model.PipelineStatistics, mod *model.Model) (uint32, uint32) {

	if parallelism == 0 { // defensive
		parallelism = 1
	}

	scheduler.instancesLock.Lock()
	defer scheduler.instancesLock.Unlock()

	currentParallelism := uint32(len(scheduler.instances))

	if atomic.LoadUint32(&scheduler.stopped) == 1 ||
		currentParallelism == ^uint32(0) { // 4294967295
		return currentParallelism, 0 // scheduler is stop or reach the cap
	}

	left := option.PipelineMaxParallelism - currentParallelism
	if parallelism > left {
		parallelism = left
	}

	idx := uint32(0)
	for idx < parallelism {
		pipeline, err := scheduler.pipeline.GetInstance(ctx, statistics, mod)
		if err != nil {
			logger.Errorf("[launch pipeline %s-#%d failed: %v]",
				scheduler.PipelineName(), currentParallelism+idx+1, err)

			return currentParallelism, idx
		}

		instance := newPipelineInstance(pipeline)
		scheduler.instances = append(scheduler.instances, instance)
		currentParallelism++

		instance.prepare()
		go instance.run()

		idx++
	}

	return currentParallelism, idx
}

func (scheduler *commonPipelineScheduler) stopPipelineInstance(idx int, instance *pipelineInstance, scheduled bool) {
	select {
	case <-instance.terminate(scheduled): // wait until stop
	case <-time.After(PIPELINE_STOP_TIMEOUT_SECONDS * time.Second):
		logger.Warnf("[stopped pipeline %s instance #%d timeout (%d seconds elapsed)]",
			scheduler.PipelineName(), idx+1, PIPELINE_STOP_TIMEOUT_SECONDS)
	}
}

func (scheduler *commonPipelineScheduler) StopPipeline() {
	logger.Debugf("[stopping pipeline %s]", scheduler.PipelineName())

	scheduler.instancesLock.Lock()
	defer scheduler.instancesLock.Unlock()

	for idx, instance := range scheduler.instances {
		scheduler.stopPipelineInstance(idx, instance, false)
	}

	currentParallelism := len(scheduler.instances)

	// no managed instance, re-entry-able function
	scheduler.instances = scheduler.instances[:0]

	logger.Infof("[stopped pipeline %s (parallelism=%d)]", scheduler.PipelineName(), currentParallelism)
}

////

const (
	SCHEDULER_DYNAMIC_SPAWN_MIN_INTERVAL_MS  = 500
	SCHEDULER_DYNAMIC_SPAWN_MAX_IN_EACH      = 500
	SCHEDULER_DYNAMIC_FAST_SCALE_INTERVAL_MS = 1000
	SCHEDULER_DYNAMIC_FAST_SCALE_RATIO       = 1.2
	SCHEDULER_DYNAMIC_FAST_SCALE_MIN_COUNT   = 5
	SCHEDULER_DYNAMIC_SHRINK_MIN_DELAY_MS    = 500
)

type inputEvent struct {
	getterName  string
	getter      pipelines.SourceInputQueueLengthGetter
	queueLength uint32
}

type dynamicPipelineScheduler struct {
	*commonPipelineScheduler
	ctx                     pipelines.PipelineContext
	statistics              *model.PipelineStatistics
	mod                     *model.Model
	gettersLock             sync.RWMutex
	getters                 map[string]pipelines.SourceInputQueueLengthGetter
	launchChan              chan *inputEvent
	spawnStop, spawnDone    chan struct{}
	shrinkStop              chan struct{}
	sourceLastScheduleTimes map[string]time.Time
	launchTimeLock          sync.RWMutex
	launchTime              time.Time
	shrinkTimeLock          sync.RWMutex
	shrinkTime              time.Time
}

func newDynamicPipelineScheduler(pipeline *model.Pipeline) *dynamicPipelineScheduler {
	return &dynamicPipelineScheduler{
		commonPipelineScheduler: newCommonPipelineScheduler(pipeline),
		getters:                 make(map[string]pipelines.SourceInputQueueLengthGetter, 1),
		launchChan:              make(chan *inputEvent, 128), // buffer for trigger() calls before scheduler starts
		spawnStop:               make(chan struct{}),
		spawnDone:               make(chan struct{}),
		shrinkStop:              make(chan struct{}),
		sourceLastScheduleTimes: make(map[string]time.Time, 1),
	}
}

func (scheduler *dynamicPipelineScheduler) SourceInputTrigger() pipelines.SourceInputTrigger {
	return scheduler.trigger
}

func (scheduler *dynamicPipelineScheduler) Start(ctx pipelines.PipelineContext,
	statistics *model.PipelineStatistics, mod *model.Model) {

	if !atomic.CompareAndSwapUint32(&scheduler.started, 0, 1) {
		return // already started
	}

	// book for delay schedule
	scheduler.ctx = ctx
	scheduler.statistics = statistics
	scheduler.mod = mod

	parallelism, _ := scheduler.startPipeline(option.PipelineInitParallelism, ctx, statistics, mod)

	logger.Debugf("[initialized pipeline instance(s) for pipeline %s (total=%d)]",
		scheduler.PipelineName(), parallelism)

	go scheduler.launch()
	go scheduler.spawn()
	go scheduler.shrink()
}

func (scheduler *dynamicPipelineScheduler) trigger(getterName string, getter pipelines.SourceInputQueueLengthGetter) {
	queueLength := getter()
	if queueLength == 0 {
		// current parallelism is enough
		return
	}

	if atomic.LoadUint32(&scheduler.stopped) == 1 {
		// scheduler is stop
		return
	}

	event := &inputEvent{
		getterName:  getterName,
		getter:      getter,
		queueLength: queueLength,
	}

	select {
	case scheduler.launchChan <- event:
	default: // skip if busy, spawn() routine will redress
	}
}

func (scheduler *dynamicPipelineScheduler) launch() {
	for {
		select {
		case info := <-scheduler.launchChan:
			if info == nil {
				return // channel/scheduler closed, exit
			}

			now := common.Now()

			if info.getterName != "" && info.getter != nil { // calls from trigger()
				lastScheduleAt := scheduler.sourceLastScheduleTimes[info.getterName]

				if now.Sub(lastScheduleAt).Seconds()*1000 < SCHEDULER_DYNAMIC_SPAWN_MIN_INTERVAL_MS {
					// pipeline instance schedule needs time
					continue
				}

				scheduler.sourceLastScheduleTimes[info.getterName] = now

				// book for spawn and shrink
				scheduler.gettersLock.Lock()
				scheduler.getters[info.getterName] = info.getter
				scheduler.gettersLock.Unlock()
			} else { // calls from spawn()
				for getterName := range scheduler.sourceLastScheduleTimes {
					scheduler.sourceLastScheduleTimes[getterName] = now
				}
			}

			scheduler.shrinkTimeLock.RLock()

			if now.Sub(scheduler.shrinkTime).Seconds()*1000 < SCHEDULER_DYNAMIC_FAST_SCALE_INTERVAL_MS {
				// increase is close to decrease, which supposes last shrink reach the real/minimal parallelism
				l := uint32(math.Ceil(float64(info.queueLength) * SCHEDULER_DYNAMIC_FAST_SCALE_RATIO)) // fast scale up
				if l < SCHEDULER_DYNAMIC_FAST_SCALE_MIN_COUNT {
					l = SCHEDULER_DYNAMIC_FAST_SCALE_MIN_COUNT
				}

				if l > info.queueLength { // defense overflow
					info.queueLength = l
				}
			}

			if info.queueLength > SCHEDULER_DYNAMIC_SPAWN_MAX_IN_EACH {
				info.queueLength = SCHEDULER_DYNAMIC_SPAWN_MAX_IN_EACH
			}

			scheduler.shrinkTimeLock.RUnlock()

			parallelism, delta := scheduler.startPipeline(
				info.queueLength, scheduler.ctx, scheduler.statistics, scheduler.mod)

			if delta > 0 {
				scheduler.launchTimeLock.Lock()
				scheduler.launchTime = common.Now()
				scheduler.launchTimeLock.Unlock()

				logger.Debugf("[spawned pipeline instance(s) for pipeline %s (total=%d, increase=%d)]",
					scheduler.PipelineName(), parallelism, delta)
			}
		}
	}
}

func (scheduler *dynamicPipelineScheduler) spawn() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer close(scheduler.spawnDone)

	for {
		select {
		case <-ticker.C:
			scheduler.instancesLock.RLock()

			currentParallelism := uint32(len(scheduler.instances))

			if currentParallelism == option.PipelineMaxParallelism {
				scheduler.instancesLock.RUnlock()
				continue // less than the cap of pipeline parallelism
			}

			scheduler.instancesLock.RUnlock()

			scheduler.gettersLock.RLock()

			var queueLength uint32
			for _, getter := range scheduler.getters {
				l := getter()
				if l+queueLength > queueLength { // defense overflow
					queueLength = l + queueLength
				}
			}

			scheduler.gettersLock.RUnlock()

			if queueLength == 0 {
				// current parallelism is enough
				continue // spawn only
			}

			scheduler.launchChan <- &inputEvent{
				queueLength: queueLength,
			} // without getterName and getter
		case <-scheduler.spawnStop:
			return
		}
	}
}

func (scheduler *dynamicPipelineScheduler) shrink() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			scheduler.instancesLock.RLock()

			currentParallelism := uint32(len(scheduler.instances))

			if currentParallelism <= option.PipelineMinParallelism {
				scheduler.instancesLock.RUnlock()
				continue // keep minimal pipeline parallelism
			}

			scheduler.instancesLock.RUnlock()

			scheduler.gettersLock.RLock()

			var queueLength uint32
			for _, getter := range scheduler.getters {
				l := getter()
				if l+queueLength > queueLength { // defense overflow
					queueLength = l + queueLength
				}
			}

			scheduler.gettersLock.RUnlock()

			if queueLength != 0 {
				continue // shrink only
			}

			var instance *pipelineInstance

			scheduler.instancesLock.Lock()

			currentParallelism = uint32(len(scheduler.instances))

			// DCL
			if currentParallelism <= option.PipelineMinParallelism {
				scheduler.instancesLock.Unlock()
				continue // keep minimal pipeline parallelism
			}

			now := common.Now()

			scheduler.launchTimeLock.RLock()

			if now.Sub(scheduler.launchTime).Seconds()*1000 < SCHEDULER_DYNAMIC_SHRINK_MIN_DELAY_MS {
				// just launched instance, need to wait it runs
				scheduler.instancesLock.Unlock()
				scheduler.launchTimeLock.RUnlock()
				continue
			}

			scheduler.launchTimeLock.RUnlock()

			// pop from tail as stack
			idx := int(currentParallelism) - 1
			instance, scheduler.instances = scheduler.instances[idx], scheduler.instances[:idx]

			scheduler.instancesLock.Unlock()

			scheduler.shrinkTimeLock.Lock()
			scheduler.shrinkTime = now
			scheduler.shrinkTimeLock.Unlock()

			scheduler.stopPipelineInstance(idx, instance, true)

			scheduler.instancesLock.RLock()

			logger.Infof("[shrank a pipeline instance for pipeline %s (total=%d, decrease=%d)]",
				scheduler.PipelineName(), len(scheduler.instances), 1)

			scheduler.instancesLock.RUnlock()
		case <-scheduler.shrinkStop:
			return
		}
	}
}

func (scheduler *dynamicPipelineScheduler) Stop() {
	if !atomic.CompareAndSwapUint32(&scheduler.stopped, 0, 1) {
		return // already stopped
	}

	close(scheduler.spawnStop)
	close(scheduler.shrinkStop)

	<-scheduler.spawnDone

	close(scheduler.launchChan)

	atomic.StoreUint32(&scheduler.started, 0)
}

////

type staticPipelineScheduler struct {
	*commonPipelineScheduler
}

func CreatePipelineScheduler(pipeline *model.Pipeline) PipelineScheduler {
	var scheduler PipelineScheduler
	if pipeline.Config().Parallelism() == 0 { // dynamic mode
		scheduler = newDynamicPipelineScheduler(pipeline)
	} else { // pre-alloc mode
		scheduler = newStaticPipelineScheduler(pipeline)
	}
	return scheduler
}

func newStaticPipelineScheduler(pipeline *model.Pipeline) *staticPipelineScheduler {
	return &staticPipelineScheduler{
		commonPipelineScheduler: newCommonPipelineScheduler(pipeline),
	}
}

func (scheduler *staticPipelineScheduler) SourceInputTrigger() pipelines.SourceInputTrigger {
	return pipelines.NoOpSourceInputTrigger
}

func (scheduler *staticPipelineScheduler) Start(ctx pipelines.PipelineContext,
	statistics *model.PipelineStatistics, mod *model.Model) {

	if !atomic.CompareAndSwapUint32(&scheduler.started, 0, 1) {
		return // already started
	}

	parallelism, _ := scheduler.startPipeline(
		uint32(scheduler.pipeline.Config().Parallelism()), ctx, statistics, mod)

	logger.Debugf("[initialized pipeline instance(s) for pipeline %s (total=%d)]",
		scheduler.PipelineName(), parallelism)
}

func (scheduler *staticPipelineScheduler) Stop() {
	if !atomic.CompareAndSwapUint32(&scheduler.stopped, 0, 1) {
		return // already stopped
	}

	atomic.StoreUint32(&scheduler.started, 0)
}