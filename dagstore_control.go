package dagstore

import (
	"context"
	"errors"
	"fmt"

	ds "github.com/ipfs/go-datastore"
)

type OpType int

const (
	OpShardRegister OpType = iota
	OpShardInitialize
	OpShardMakeAvailable
	OpShardDestroy
	OpShardAcquire
	OpShardFail
	OpShardRecover
)

func (o OpType) String() string {
	return [...]string{
		"OpShardRegister",
		"OpShardInitialize",
		"OpShardMakeAvailable",
		"OpShardDestroy",
		"OpShardAcquire",
		"OpShardFail",
		"OpShardRecover"}[o]
}

// control runs the DAG store's event loop.
func (d *DAGStore) control() {
	defer d.wg.Done()

	// wFailure is a synthetic failure waiter that uses the DAGStore's
	// global context and the failure channel. Only safe to actually use if
	// d.failureCh != nil. wFailure is used to dispatch failure
	// notifications to the application.
	var wFailure = &waiter{ctx: d.ctx, outCh: d.failureCh}

	for {
		// consume the next task; if we're shutting down, this method will error.
		tsk, err := d.consumeNext()
		if err != nil {
			if err == context.Canceled {
				log.Infow("dagstore closed")
			} else {
				log.Errorw("consuming next task failed; aborted event loop; dagstore unoperational", "error", err)
			}
			return
		}

		s := tsk.shard
		log.Debugw("processing task", "op", tsk.op, "shard", tsk.shard.key, "error", tsk.err)

		persist := true
		s.lk.Lock()
		prevState := s.state

		switch tsk.op {
		case OpShardRegister:
			if s.state != ShardStateNew {
				// sanity check failed
				_ = d.failShard(s, d.internalCh, "%w: expected shard to be in 'new' state; was: %s", ErrShardInitializationFailed, s.state)
				break
			}

			// skip initialization if shard was registered with lazy init, and
			// respond immediately to waiter.
			if s.lazy {
				log.Debugw("shard registered with lazy initialization", "shard", s.key)
				// waiter will be nil if this was a restart and not a call to Register() call.
				if tsk.waiter != nil {
					res := &ShardResult{Key: s.key}
					d.dispatchResult(res, tsk.waiter)
				}
				break
			}

			// otherwise, park the registration channel and queue the init.
			s.wRegister = tsk.waiter
			_ = d.queueTask(&task{op: OpShardInitialize, shard: s, waiter: tsk.waiter}, d.internalCh)

		case OpShardInitialize:
			// if we already have the index for this shard, there's nothing to do here.
			if istat, err := d.indices.StatFullIndex(s.key); err == nil && istat.Exists {
				log.Debugw("already have an index for shard being initialized, nothing to do", "shard", s.key)
				_ = d.queueTask(&task{op: OpShardMakeAvailable, shard: s}, d.internalCh)
				break
			}

			go d.initializeShard(tsk.ctx, s, s.mount)

		case OpShardMakeAvailable:
			// can arrive here after initializing a new shard,
			// or when recovering from a failure.

			s.state = ShardStateAvailable
			s.err = nil // nillify past errors

			// notify the registration waiter, if there is one.
			if s.wRegister != nil {
				res := &ShardResult{Key: s.key}
				d.dispatchResult(res, s.wRegister)
				s.wRegister = nil
			}

			// notify the recovery waiter, if there is one.
			if s.wRecover != nil {
				res := &ShardResult{Key: s.key}
				d.dispatchResult(res, s.wRecover)
				s.wRecover = nil
			}

			// trigger queued acquisition waiters.
			for _, w := range s.wAcquire {
				go d.acquireAsync(w.ctx, w, s, s.mount)
			}
			s.wAcquire = s.wAcquire[:0]

		case OpShardAcquire:
			err = d.shardFromPersistentState(s)
			if err != nil {
				err := fmt.Errorf("could not acquire shard: %w", err)
				res := &ShardResult{Key: s.key, Error: err}
				d.dispatchResult(res, tsk.waiter)
				persist = false
				break
			}

			log.Debugw("got request to acquire shard", "shard", s.key, "current shard state", s.state)
			w := &waiter{ctx: tsk.ctx, outCh: tsk.outCh}

			// if the shard is errored, fail the acquire immediately.
			if s.state == ShardStateErrored {
				if s.recoverOnNextAcquire {
					// we are errored, but recovery was requested on the next acquire
					// we park the acquirer and trigger a recover.
					s.wAcquire = append(s.wAcquire, w)
					s.recoverOnNextAcquire = false
					// we use the global context instead of the acquire context
					// to avoid the first context cancellation interrupting the
					// recovery that may be blocking other acquirers with longer
					// contexts.
					_ = d.queueTask(&task{op: OpShardRecover, shard: s, waiter: &waiter{ctx: d.ctx}}, d.internalCh)
				} else {
					err := fmt.Errorf("shard is in errored state; err: %w", s.err)
					res := &ShardResult{Key: s.key, Error: err}
					d.dispatchResult(res, w)
				}
				break
			}

			if s.state != ShardStateAvailable {
				log.Debugw("shard isn't active yet, will queue acquire channel", "shard", s.key)
				// shard state isn't active yet; make this acquirer wait.
				s.wAcquire = append(s.wAcquire, w)

				// if the shard was registered with lazy init, and this is the
				// first acquire, queue the initialization.
				if s.state == ShardStateNew {
					log.Debugw("acquiring shard with lazy init enabled, will queue shard initialization", "shard", s.key)
					// Override the context with the background context.
					// We can't use the acquirer's context for initialization
					// because there can be multiple concurrent acquirers, and
					// if the first one cancels, the entire job would be cancelled.
					w := *tsk.waiter
					w.ctx = context.Background()
					_ = d.queueTask(&task{op: OpShardInitialize, shard: s, waiter: &w}, d.internalCh)
				}

				break
			}

			go d.acquireAsync(tsk.ctx, w, s, s.mount)

		case OpShardFail:
			s.state = ShardStateErrored
			s.err = tsk.err

			// notify the registration waiter, if there is one.
			if s.wRegister != nil {
				res := &ShardResult{
					Key:   s.key,
					Error: fmt.Errorf("failed to register shard: %w", tsk.err),
				}
				d.dispatchResult(res, s.wRegister)
				s.wRegister = nil
			}

			// notify the recovery waiter, if there is one.
			if s.wRecover != nil {
				res := &ShardResult{
					Key:   s.key,
					Error: fmt.Errorf("failed to recover shard: %w", tsk.err),
				}
				d.dispatchResult(res, s.wRecover)
				s.wRecover = nil
			}

			// fail waiting acquirers.
			// can't block the event loop, so launch a goroutine per acquirer.
			if len(s.wAcquire) > 0 {
				err := fmt.Errorf("failed to acquire shard: %w", tsk.err)
				res := &ShardResult{Key: s.key, Error: err}
				d.dispatchResult(res, s.wAcquire...)
				s.wAcquire = s.wAcquire[:0] // clear acquirers.
			}

			// Should we interrupt/disturb active acquirers? No.
			//
			// This part doesn't know which kind of error occurred.
			// It could be that the index has disappeared for new acquirers, but
			// active acquirers already have it.
			//
			// If this is a physical error (e.g. shard data was physically
			// deleted, or corrupted), we'll leave to the ShardAccessor (and the
			// ReadBlockstore) to fail at some point. At that stage, the caller
			// will call ShardAccessor#Close and eventually all active
			// references will be released, setting the shard in an errored
			// state with zero refcount.

			// Notify the application of the failure, if they provided a channel.
			if ch := d.failureCh; ch != nil {
				res := &ShardResult{Key: s.key, Error: s.err}
				d.dispatchFailuresCh <- &dispatch{res: res, w: wFailure}
			}

		case OpShardRecover:
			err = d.shardFromPersistentState(s)
			if err != nil {
				err := fmt.Errorf("could not recover shard: %w", err)
				res := &ShardResult{Key: s.key, Error: err}
				d.dispatchResult(res, tsk.waiter)
				persist = false
				break
			}
			if s.state != ShardStateErrored {
				err := fmt.Errorf("refused to recover shard in state other than errored; current state: %d", s.state)
				res := &ShardResult{Key: s.key, Error: err}
				d.dispatchResult(res, tsk.waiter)
				break
			}

			// park the waiter
			s.wRecover = tsk.waiter

			// fetch again and reindex.
			go d.initializeShard(tsk.ctx, s, s.mount)

		case OpShardDestroy:
			persist = false
			if err := d.store.Delete(d.ctx, ds.NewKey(s.key.String())); err != nil && !errors.Is(err, ds.ErrNotFound) {
				err := fmt.Errorf("failed to delete shard %s: %w", s.key, err)
				res := &ShardResult{Key: s.key, Error: err}
				d.dispatchResult(res, tsk.waiter)
				break
			}

		default:
			panic(fmt.Sprintf("unrecognized shard operation: %d", tsk.op))

		}

		if persist {
			// persist the current shard state.
			if err := s.persist(d.ctx, d.store); err != nil { // TODO maybe fail shard?
				log.Warnw("failed to persist shard", "shard", s.key, "error", err)
			}
		}

		// send a notification if the user provided a notification channel.
		if d.traceCh != nil {
			log.Debugw("will write trace to the trace channel", "shard", s.key)
			n := Trace{
				Key: s.key,
				Op:  tsk.op,
				After: ShardInfo{
					ShardState: s.state,
					Error:      s.err,
				},
			}
			d.traceCh <- n
			log.Debugw("finished writing trace to the trace channel", "shard", s.key)
		}

		log.Debugw("finished processing task", "op", tsk.op, "shard", tsk.shard.key, "prev_state", prevState, "curr_state", s.state, "error", tsk.err)

		s.lk.Unlock()
	}
}

func (d *DAGStore) consumeNext() (tsk *task, error error) {
	select {
	case tsk = <-d.internalCh: // drain internal first; these are tasks emitted from the event loop.
		return tsk, nil
	case <-d.ctx.Done():
		return nil, d.ctx.Err() // TODO drain and process before returning?
	default:
	}

	select {
	case tsk = <-d.externalCh:
		return tsk, nil
	case tsk = <-d.completionCh:
		return tsk, nil
	case <-d.ctx.Done():
		return nil, d.ctx.Err() // TODO drain and process before returning?
	}
}
