package lib

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/gravitational/teleport-plugins/lib/logger"
	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/trace"

	"google.golang.org/grpc"
)

type WatcherJobFunc func(context.Context, types.Event) error

type WatcherJobConfig struct {
	Watch          types.Watch
	MaxConcurrency int
	EventQueueSize int
}

type watcherJob struct {
	ServiceJob
	client    *client.Client
	config    WatcherJobConfig
	eventFunc WatcherJobFunc
	queues    map[watcherQueueKey]*watcherQueue
	queueMu   sync.Mutex
}

type watcherQueueKey struct {
	kind string
	name string
}

type watcherQueue struct {
	key  watcherQueueKey
	ch   chan types.Event
	busy int
}

func NewWatcherJob(client *client.Client, config WatcherJobConfig, fn WatcherJobFunc) ServiceJob {
	client = client.WithCallOptions(grpc.WaitForReady(true)) // Enable backoff on reconnecting.
	if config.MaxConcurrency == 0 {
		config.MaxConcurrency = runtime.GOMAXPROCS(0)
	}
	if config.EventQueueSize == 0 {
		config.EventQueueSize = 32
	}
	watcherJob := &watcherJob{
		client:    client,
		config:    config,
		eventFunc: fn,
		queues:    make(map[watcherQueueKey]*watcherQueue),
	}
	watcherJob.ServiceJob = NewServiceJob(func(ctx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		log := logger.Get(ctx)

		MustGetProcess(ctx).OnTerminate(func(_ context.Context) error {
			cancel()
			return nil
		})

		for {
			err := watcherJob.eventLoop(ctx)
			switch {
			case trace.IsConnectionProblem(err):
				log.WithError(err).Error("Failed to connect to Teleport Auth server. Reconnecting...")
			case trace.IsEOF(err):
				log.WithError(err).Error("Watcher stream closed. Reconnecting...")
			case IsCanceled(err):
				// Context cancellation is not an error
				return nil
			default:
				return trace.Wrap(err)
			}
		}
	})
	return watcherJob
}

// eventLoop reads the events from watcher and spawns temporary micro-queues.
func (job *watcherJob) eventLoop(ctx context.Context) error {
	watcher, err := job.client.NewWatcher(ctx, job.config.Watch)
	if err != nil {
		return trace.Wrap(err)
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			logger.Get(ctx).WithError(err).Error("Failed to close a watcher")
		}
	}()

	if err := job.waitInit(ctx, watcher, 5*time.Second); err != nil {
		return trace.Wrap(err)
	}

	logger.Get(ctx).Debug("Watcher connected")
	job.SetReady(true)

	for {
		select {
		case event := <-watcher.Events():
			if err := job.enqueue(ctx, event); err != nil {
				logger.Get(ctx).WithError(err).Error("Failed to enqueue an event")
			}
		case <-watcher.Done():
			return trace.Wrap(watcher.Error())
		}
	}
}

// enqueue puts an event to a temporary micro-queue allocated for each resource to maintain the event order.
func (job *watcherJob) enqueue(ctx context.Context, event types.Event) error {
	queueKey := watcherQueueKey{kind: event.Resource.GetKind(), name: event.Resource.GetName()}

	var (
		queue *watcherQueue
		exist bool
	)
	for {
		if err := ctx.Err(); err != nil {
			return trace.Wrap(err)
		}

		job.queueMu.Lock()
		if queue, exist = job.queues[queueKey]; !exist {
			if len(job.queues) >= job.config.MaxConcurrency {
				job.queueMu.Unlock()
				continue
			}

			queue = &watcherQueue{
				key: queueKey,
				ch:  make(chan types.Event, job.config.EventQueueSize),
			}

			job.queues[queueKey] = queue
		}
		queue.busy++
		job.queueMu.Unlock()
		break
	}

	queue.ch <- event

	if !exist {
		MustGetProcess(ctx).Spawn(func(ctx context.Context) error { return job.queueEventLoop(ctx, queue) })
	}

	return nil
}

// queueEventLoop dispatches a temporary micro-queue for a given resource.
func (job *watcherJob) queueEventLoop(ctx context.Context, queue *watcherQueue) error {
	defer job.tryDisposeQueue(queue)
	for {
		select {
		case event := <-queue.ch:
			job.queueMu.Lock()
			queue.busy--
			job.queueMu.Unlock()

			_ = job.eventFunc(ctx, event)
		default:
			if job.tryDisposeQueue(queue) {
				return nil
			}
		}
	}
}

// tryDisposeQueue destroys the temporary micro-queue if it's empty.
func (job *watcherJob) tryDisposeQueue(queue *watcherQueue) bool {
	job.queueMu.Lock()
	defer job.queueMu.Unlock()

	if queue.busy == 0 {
		delete(job.queues, queue.key)
		return true
	}

	return false
}

// waitInit waits for OpInit event be received on a stream.
func (job *watcherJob) waitInit(ctx context.Context, watcher types.Watcher, timeout time.Duration) error {
	select {
	case event := <-watcher.Events():
		if event.Type != types.OpInit {
			return trace.ConnectionProblem(nil, "unexpected event type %q", event.Type)
		}
		return nil
	case <-time.After(timeout):
		return trace.ConnectionProblem(nil, "watcher initialization timed out")
	case <-watcher.Done():
		return trace.Wrap(watcher.Error())
	case <-ctx.Done():
		return trace.Wrap(ctx.Err())
	}
}
