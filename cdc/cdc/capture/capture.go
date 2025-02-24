// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package capture

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	tidbkv "github.com/pingcap/tidb/kv"
	"github.com/tikv/client-go/v2/tikv"
	pd "github.com/tikv/pd/client"
	"go.etcd.io/etcd/client/v3/concurrency"
	"go.etcd.io/etcd/server/v3/mvcc"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/tikv/migration/cdc/cdc/kv"
	"github.com/tikv/migration/cdc/cdc/model"
	"github.com/tikv/migration/cdc/cdc/owner"
	"github.com/tikv/migration/cdc/cdc/processor"
	"github.com/tikv/migration/cdc/pkg/config"
	cdcContext "github.com/tikv/migration/cdc/pkg/context"
	cerror "github.com/tikv/migration/cdc/pkg/errors"
	"github.com/tikv/migration/cdc/pkg/etcd"
	"github.com/tikv/migration/cdc/pkg/orchestrator"
	"github.com/tikv/migration/cdc/pkg/pdtime"
	"github.com/tikv/migration/cdc/pkg/version"
)

type createEtcdClientFunc func(ctx context.Context) (*etcd.CDCEtcdClient, error)

// Capture represents a Capture server, it monitors the changefeed information in etcd and schedules Task on it.
type Capture struct {
	captureMu sync.Mutex
	info      *model.CaptureInfo

	ownerMu          sync.Mutex
	owner            *owner.Owner
	processorManager *processor.Manager

	// session keeps alive between the capture and etcd
	session  *concurrency.Session
	election *concurrency.Election

	pdClient         pd.Client
	kvStorage        tidbkv.Storage
	createEtcdClient createEtcdClientFunc
	etcdClient       *etcd.CDCEtcdClient
	grpcPool         kv.GrpcPool
	regionCache      *tikv.RegionCache
	TimeAcquirer     pdtime.TimeAcquirer

	cancel context.CancelFunc

	newProcessorManager func() *processor.Manager
	newOwner            func(pd.Client) *owner.Owner
}

// NewCapture returns a new Capture instance
func NewCapture(pdClient pd.Client, kvStorage tidbkv.Storage, createEtcdClient createEtcdClientFunc) *Capture {
	return &Capture{
		pdClient:         pdClient,
		kvStorage:        kvStorage,
		createEtcdClient: createEtcdClient,
		cancel:           func() {},

		newProcessorManager: processor.NewManager,
		newOwner:            owner.NewOwner,
	}
}

func NewCapture4Test() *Capture {
	return &Capture{
		info: &model.CaptureInfo{ID: "capture-for-test", AdvertiseAddr: "127.0.0.1", Version: "test"},
	}
}

func (c *Capture) reset(ctx context.Context) error {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()

	etcdClient, err := c.createEtcdClient(ctx)
	if err != nil {
		return errors.Annotate(
			cerror.WrapError(cerror.ErrNewCaptureFailed, err),
			"create etcd client")
	}
	c.etcdClient = etcdClient

	conf := config.GetGlobalServerConfig()
	c.info = &model.CaptureInfo{
		ID:            uuid.New().String(),
		AdvertiseAddr: conf.AdvertiseAddr,
		Version:       version.ReleaseVersion,
	}
	c.processorManager = c.newProcessorManager()
	if c.session != nil {
		// It can't be handled even after it fails, so we ignore it.
		_ = c.session.Close()
	}

	// NewSession without lease will block, when send SIGSTOP to pd leader.
	// So we should grant lease first.
	leaseID, err := c.etcdClient.Client.Grant(ctx, int64(conf.CaptureSessionTTL))
	if err != nil {
		return errors.Annotate(
			cerror.WrapError(cerror.ErrNewCaptureFailed, err),
			"grant lease")
	}

	sess, err := concurrency.NewSession(c.etcdClient.Client.Unwrap(),
		concurrency.WithTTL(conf.CaptureSessionTTL),
		concurrency.WithLease(leaseID.ID))
	if err != nil {
		return errors.Annotate(
			cerror.WrapError(cerror.ErrNewCaptureFailed, err),
			"create capture session")
	}
	c.session = sess
	c.election = concurrency.NewElection(sess, etcd.CaptureOwnerKey)

	if c.TimeAcquirer != nil {
		c.TimeAcquirer.Stop()
	}
	c.TimeAcquirer = pdtime.NewTimeAcquirer(c.pdClient)
	if c.grpcPool != nil {
		c.grpcPool.Close()
	}

	c.grpcPool = kv.NewGrpcPoolImpl(ctx, conf.Security)
	if c.regionCache != nil {
		c.regionCache.Close()
	}
	c.regionCache = tikv.NewRegionCache(c.pdClient)

	log.Info("init capture",
		zap.String("capture-id", c.info.ID),
		zap.String("capture-addr", c.info.AdvertiseAddr))
	return nil
}

// Run runs the capture
func (c *Capture) Run(ctx context.Context) error {
	defer log.Info("the capture routine has exited")
	// Limit the frequency of reset capture to avoid frequent recreating of resources
	rl := rate.NewLimiter(0.05, 2)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		ctx, cancel := context.WithCancel(ctx)
		c.cancel = cancel
		err := rl.Wait(ctx)
		if err != nil {
			if errors.Cause(err) == context.Canceled {
				return nil
			}
			return errors.Trace(err)
		}
		err = c.reset(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		err = c.run(ctx)
		// if capture suicided, reset the capture and run again.
		// if the canceled error throw, there are two possible scenarios:
		//   1. the internal context canceled, it means some error happened in the internal, and the routine is exited, we should restart the capture
		//   2. the parent context canceled, it means that the caller of the capture hope the capture to exit, and this loop will return in the above `select` block
		// TODO: make sure the internal cancel should return the real error instead of context.Canceled
		if cerror.ErrCaptureSuicide.Equal(err) || context.Canceled == errors.Cause(err) {
			log.Info("capture recovered", zap.String("capture-id", c.info.ID))
			continue
		}
		return errors.Trace(err)
	}
}

func (c *Capture) run(stdCtx context.Context) error {
	ctx := cdcContext.NewContext(stdCtx, &cdcContext.GlobalVars{
		PDClient:     c.pdClient,
		KVStorage:    c.kvStorage,
		CaptureInfo:  c.info,
		EtcdClient:   c.etcdClient,
		GrpcPool:     c.grpcPool,
		RegionCache:  c.regionCache,
		TimeAcquirer: c.TimeAcquirer,
	})
	err := c.register(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	wg := new(sync.WaitGroup)
	var ownerErr, processorErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer c.AsyncClose()
		// when the campaignOwner returns an error, it means that the owner throws an unrecoverable serious errors
		// (recoverable errors are intercepted in the owner tick)
		// so we should also stop the owner and let capture restart or exit
		ownerErr = c.campaignOwner(ctx)
		log.Info("the owner routine has exited", zap.Error(ownerErr))
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer c.AsyncClose()

		conf := config.GetGlobalServerConfig()
		processorFlushInterval := time.Duration(conf.ProcessorFlushInterval)
		globalState := orchestrator.NewGlobalState()

		// when the etcd worker of processor returns an error, it means that the processor throws an unrecoverable serious errors
		// (recoverable errors are intercepted in the processor tick)
		// so we should also stop the processor and let capture restart or exit
		processorErr = c.runEtcdWorker(ctx, c.processorManager, globalState, processorFlushInterval)
		log.Info("the processor routine has exited", zap.Error(processorErr))
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.TimeAcquirer.Run(ctx)
		log.Info("the time acquirer routine has exited")
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.grpcPool.RecycleConn(ctx)
		log.Info("the grpcPoll routine has exited")
	}()
	wg.Wait()
	if ownerErr != nil {
		return errors.Annotate(ownerErr, "owner exited with error")
	}
	if processorErr != nil {
		return errors.Annotate(processorErr, "processor exited with error")
	}
	return nil
}

// Info gets the capture info
func (c *Capture) Info() model.CaptureInfo {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	return *c.info
}

func (c *Capture) campaignOwner(ctx cdcContext.Context) error {
	// In most failure cases, we don't return error directly, just run another
	// campaign loop. We treat campaign loop as a special background routine.
	conf := config.GetGlobalServerConfig()
	ownerFlushInterval := time.Duration(conf.OwnerFlushInterval)
	failpoint.Inject("ownerFlushIntervalInject", func(val failpoint.Value) {
		ownerFlushInterval = time.Millisecond * time.Duration(val.(int))
	})
	// Limit the frequency of elections to avoid putting too much pressure on the etcd server
	rl := rate.NewLimiter(0.05, 2)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		err := rl.Wait(ctx)
		if err != nil {
			if errors.Cause(err) == context.Canceled {
				return nil
			}
			return errors.Trace(err)
		}
		// Campaign to be an owner, it blocks until it becomes the owner
		if err := c.campaign(ctx); err != nil {
			switch errors.Cause(err) {
			case context.Canceled:
				return nil
			case mvcc.ErrCompacted:
				// the revision we requested is compacted, just retry
				continue
			}
			log.Warn("campaign owner failed", zap.Error(err))
			// if campaign owner failed, restart capture
			return cerror.ErrCaptureSuicide.GenWithStackByArgs()
		}

		ownerRev, err := c.etcdClient.GetOwnerRevision(ctx, c.info.ID)
		if err != nil {
			if errors.Cause(err) == context.Canceled {
				return nil
			}
			return errors.Trace(err)
		}

		// We do a copy of the globalVars here to avoid
		// accidental modifications and potential race conditions.
		globalVars := *ctx.GlobalVars()
		newGlobalVars := &globalVars
		newGlobalVars.OwnerRevision = ownerRev
		ownerCtx := cdcContext.NewContext(ctx, newGlobalVars)

		log.Info("campaign owner successfully",
			zap.String("capture-id", c.info.ID),
			zap.Int64("owner-rev", ownerRev))

		owner := c.newOwner(c.pdClient)
		c.setOwner(owner)
		err = c.runEtcdWorker(ownerCtx, owner, orchestrator.NewGlobalState(), ownerFlushInterval)
		c.setOwner(nil)
		log.Info("run owner exited", zap.Error(err))

		// TODO: fix invalid resign
		// When exiting normally, cancel will be called to make `owner routine`
		// & `processor routine` exit normally.
		//
		// For `owner routine`, when cancel is called, `owner routine` will return
		// from `runEtcdWorker` and call `resign`.
		//
		// But now ctx is cancel, so resign will not work.
		//
		// More detail, https://github.com/pingcap/tiflow/pull/6284
		if resignErr := c.resign(ctx); resignErr != nil {
			// if resigning owner failed, return error to let capture exits
			return errors.Annotatef(resignErr, "resign owner failed, capture: %s", c.info.ID)
		}
		if err != nil {
			// for errors, return error and let capture exits or restart
			return errors.Trace(err)
		}
		// if owner exits normally, continue the campaign loop and try to election owner again
	}
}

func (c *Capture) runEtcdWorker(ctx cdcContext.Context, reactor orchestrator.Reactor, reactorState orchestrator.ReactorState, timerInterval time.Duration) error {
	etcdWorker, err := orchestrator.NewEtcdWorker(ctx.GlobalVars().EtcdClient.Client, etcd.EtcdKeyBase, reactor, reactorState)
	if err != nil {
		return errors.Trace(err)
	}
	captureAddr := c.info.AdvertiseAddr
	if err := etcdWorker.Run(ctx, c.session, timerInterval, captureAddr); err != nil {
		// We check ttl of lease instead of check `session.Done`, because
		// `session.Done` is only notified when etcd client establish a
		// new keepalive request, there could be a time window as long as
		// 1/3 of session ttl that `session.Done` can't be triggered even
		// the lease is already revoked.
		switch {
		case cerror.ErrEtcdSessionDone.Equal(err),
			cerror.ErrLeaseExpired.Equal(err):
			log.Warn("session is disconnected", zap.Error(err))
			return cerror.ErrCaptureSuicide.GenWithStackByArgs()
		}
		lease, inErr := ctx.GlobalVars().EtcdClient.Client.TimeToLive(ctx, c.session.Lease())
		if inErr != nil {
			return cerror.WrapError(cerror.ErrPDEtcdAPIError, inErr)
		}
		if lease.TTL == int64(-1) {
			log.Warn("session is disconnected", zap.Error(err))
			return cerror.ErrCaptureSuicide.GenWithStackByArgs()
		}
		return errors.Trace(err)
	}
	return nil
}

func (c *Capture) setOwner(owner *owner.Owner) {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	c.owner = owner
}

// OperateOwnerUnderLock operates the owner with lock
func (c *Capture) OperateOwnerUnderLock(fn func(*owner.Owner) error) error {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	if c.owner == nil {
		return cerror.ErrNotOwner.GenWithStackByArgs()
	}
	return fn(c.owner)
}

// campaign to be an owner.
func (c *Capture) campaign(ctx cdcContext.Context) error {
	failpoint.Inject("capture-campaign-compacted-error", func() {
		failpoint.Return(errors.Trace(mvcc.ErrCompacted))
	})
	return cerror.WrapError(cerror.ErrCaptureCampaignOwner, c.election.Campaign(ctx, c.info.ID))
}

// resign lets an owner start a new election.
func (c *Capture) resign(ctx cdcContext.Context) error {
	failpoint.Inject("capture-resign-failed", func() {
		failpoint.Return(errors.New("capture resign failed"))
	})
	return cerror.WrapError(cerror.ErrCaptureResignOwner, c.election.Resign(ctx))
}

// register registers the capture information in etcd
func (c *Capture) register(ctx cdcContext.Context) error {
	err := ctx.GlobalVars().EtcdClient.PutCaptureInfo(ctx, c.info, c.session.Lease())
	if err != nil {
		return cerror.WrapError(cerror.ErrCaptureRegister, err)
	}
	return nil
}

// AsyncClose closes the capture by unregistering it from etcd
// Note: this function should be reentrant
func (c *Capture) AsyncClose() {
	defer c.cancel()
	// Safety: Here we mainly want to stop the owner
	// and ignore it if the owner does not exist or is not set.

	_ = c.OperateOwnerUnderLock(func(o *owner.Owner) error {
		o.AsyncStop()
		return nil
	})
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	if c.processorManager != nil {
		c.processorManager.AsyncClose()
	}
	if c.grpcPool != nil {
		c.grpcPool.Close()
	}
	if c.regionCache != nil {
		c.regionCache.Close()
		c.regionCache = nil
	}

	if c.etcdClient != nil {
		timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.etcdClient.DeleteCaptureInfo(timeoutCtx, c.info.ID); err != nil {
			log.Warn("failed to delete capture info when capture exited", zap.Error(err))
		}
		cancel()

		if err := c.etcdClient.Close(); err != nil {
			log.Warn("failed to close etcd client", zap.Error(err))
		}
		c.etcdClient = nil
	}
}

// WriteDebugInfo writes the debug info into writer.
func (c *Capture) WriteDebugInfo(w io.Writer) {
	// Safety: Because we are mainly outputting information about the owner here,
	// if the owner does not exist or is not set, the information will not be output.
	_ = c.OperateOwnerUnderLock(func(o *owner.Owner) error {
		fmt.Fprintf(w, "\n\n*** owner info ***:\n\n")
		o.WriteDebugInfo(w)
		return nil
	})
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	if c.processorManager != nil {
		fmt.Fprintf(w, "\n\n*** processors info ***:\n\n")
		c.processorManager.WriteDebugInfo(w)
	}
}

// IsOwner returns whether the capture is an owner
func (c *Capture) IsOwner() bool {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	return c.owner != nil
}

// GetOwner return the owner of current TiCDC cluster
func (c *Capture) GetOwner(ctx context.Context) (*model.CaptureInfo, error) {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	if c.etcdClient == nil {
		return nil, errors.Errorf("Capture is not ready")
	}

	_, captureInfos, err := c.etcdClient.GetCaptures(ctx)
	if err != nil {
		return nil, err
	}

	ownerID, err := c.etcdClient.GetOwnerID(ctx, etcd.CaptureOwnerKey)
	if err != nil {
		return nil, err
	}

	for _, captureInfo := range captureInfos {
		if captureInfo.ID == ownerID {
			return captureInfo, nil
		}
	}
	return nil, cerror.ErrOwnerNotFound.FastGenByArgs()
}

// CreateChangefeddInfo put new changefeed info in etcd
func (c *Capture) CreateChangefeedInfo(ctx context.Context, info *model.ChangeFeedInfo, cfID string) error {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	if c.etcdClient == nil {
		return errors.Errorf("Capture is not ready")
	}

	return c.etcdClient.CreateChangefeedInfo(ctx, info, cfID)
}

// SaveChangFeedInfo update changefeed info in etcd
func (c *Capture) SaveChangeFeedInfo(ctx context.Context, info *model.ChangeFeedInfo, cfID string) error {
	c.captureMu.Lock()
	defer c.captureMu.Unlock()
	if c.etcdClient == nil {
		return errors.Errorf("Capture is not ready")
	}
	return c.etcdClient.SaveChangeFeedInfo(ctx, info, cfID)
}
