package etcdadapter

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/api7/etcd-adapter/cache"
)

// EventType is the type of event kind.
type EventType int

const (
	// EventAdd is the add event.
	EventAdd = EventType(iota + 1)
	// EventUpdate is the update event.
	EventUpdate
	// EventDelete is the delete event
	EventDelete
)

var (
	_errInternalError = status.New(codes.Internal, "internal error").Err()
)

// cacheItem wraps the cache.item with some etcd specific concepts
// like create revision and modify revision.
type cacheItem struct {
	cache.Item

	createRevision int64
	modRevision    int64
}

// MarshalLogObject implements the zapcore.ObjectMarshal interface.
// TODO performance benchmark.
func (item *cacheItem) MarshalLogObject(oe zapcore.ObjectEncoder) error {
	oe.AddInt64("create_revision", item.createRevision)
	oe.AddInt64("modify_revision", item.modRevision)
	if err := oe.AddReflected("value", item.Item); err != nil {
		return err
	}
	return nil
}

// Event contains a bunch of entities and the type of event.
type Event struct {
	// Item is the slice of event entites.
	Items []cache.Item
	// Type is the event type.
	Type EventType
}

// itemKey implements the cache.Item interface.
type itemKey string

func (ik itemKey) Key() string {
	return string(ik)
}

func (ik itemKey) Marshal() ([]byte, error) {
	return []byte(ik), nil
}

type Adapter interface {
	// EventCh returns a send-only channel to the users, so that users
	// can feed events to Etcd Adapter. Note this is a non-buffered channel.
	EventCh() chan<- *Event
	// Serve accepts a net.Listener object and starts the Etcd V3 server.
	Serve(context.Context, net.Listener) error
	// Shutdown shuts the etcd adapter down.
	Shutdown(context.Context) error
}

type adapter struct {
	revision int64
	ctx      context.Context

	logger  *zap.Logger
	grpcSrv *grpc.Server
	httpSrv *http.Server

	eventsCh chan *Event
	cache    cache.Cache
}

type AdapterOptions struct {
	logger *zap.Logger
}

// NewEtcdAdapter new an etcd adapter instance.
func NewEtcdAdapter(opts *AdapterOptions) Adapter {
	a := &adapter{
		eventsCh: make(chan *Event),
		cache:    cache.NewBTreeCache(),
	}
	if opts != nil && opts.logger != nil {
		a.logger = opts.logger
	} else {
		a.logger = zap.NewExample()
	}
	return a
}

func (a *adapter) EventCh() chan<- *Event {
	return a.eventsCh
}

func (a *adapter) watchEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-a.eventsCh:
			for _, it := range ev.Items {
				rev := a.incrRevision()
				ci := &cacheItem{
					Item:        it,
					modRevision: rev,
				}
				switch ev.Type {
				case EventAdd:
					ci.createRevision = rev
					a.cache.Put(ci)
					a.logger.Debug("add event received",
						zap.Object("item", ci),
					)
				case EventUpdate:
					if old := a.cache.Get(it); old != nil {
						ci.createRevision = old.(*cacheItem).createRevision
						a.cache.Put(ci)
						a.logger.Debug("update event received",
							zap.Object("item", ci),
						)
					} else {
						a.logger.Error("ignore update event as object is not found from the cache",
							zap.Object("item", ci),
						)
					}
				case EventDelete:
					if old := a.cache.Get(it); old != nil {
						ci.createRevision = old.(*cacheItem).createRevision
						a.cache.Delete(ci)
						a.logger.Debug("delete event received",
							zap.Object("item", ci),
						)
					} else {
						a.logger.Error("ignore delete event as object is not found from the cache",
							zap.Object("item", ci),
						)
					}
				}
				// TODO pass ci to etcd server.
			}
		}
	}
}

func (a *adapter) incrRevision() int64 {
	old := atomic.LoadInt64(&a.revision)
	for {
		if atomic.CompareAndSwapInt64(&a.revision, old, old+1) {
			return old + 1
		}
	}
}

func (a *adapter) showVersion(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`{"etcdserver":"3.5.0-pre","etcdcluster":"3.5.0"}`))
	if err != nil {
		a.logger.Warn("failed to send version info",
			zap.Error(err),
		)
	}
}
