// +build !community

package indexer

// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/common/queryutil"
	"github.com/couchbase/indexing/secondary/logging"
	"github.com/couchbase/plasma"
)

// Note - CE builds do not pull in plasma_slice.go
// Do not put any shared variables here

func init() {
	plasma.SetLogger(&logging.SystemLogger)
}

type plasmaSlice struct {
	newBorn                               bool
	get_bytes, insert_bytes, delete_bytes int64
	flushedCount                          uint64
	committedCount                        uint64
	qCount                                int64

	path string
	id   SliceId

	refCount int
	lock     sync.RWMutex

	mainstore *plasma.Plasma
	backstore *plasma.Plasma

	main []*plasma.Writer

	back []*plasma.Writer

	readers chan *plasma.Reader

	idxDefn    common.IndexDefn
	idxDefnId  common.IndexDefnId
	idxInstId  common.IndexInstId
	idxPartnId common.PartitionId

	status        SliceStatus
	isActive      bool
	isDirty       bool
	isPrimary     bool
	isSoftDeleted bool
	isSoftClosed  bool
	numPartitions int
	isCompacting  bool

	cmdCh  []chan indexMutation
	stopCh []DoneChannel

	workerDone []chan bool

	fatalDbErr error

	numWriters    int
	maxNumWriters int
	maxRollbacks  int

	totalFlushTime  time.Duration
	totalCommitTime time.Duration

	idxStats *IndexStats
	sysconf  common.Config
	confLock sync.RWMutex

	isPersistorActive int32

	// Array processing
	arrayExprPosition int
	isArrayDistinct   bool

	encodeBuf [][]byte
	arrayBuf1 [][]byte
	arrayBuf2 [][]byte

	hasPersistence bool

	indexerStats *IndexerStats

	//
	// The following fields are used for tuning writers
	//

	// stats sampling
	drainTime     int64          // elapsed time for draining
	numItems      int64          // num of items in each flush
	drainRate     *common.Sample // samples of drain rate per writer (numItems per writer per flush interval)
	mutationRate  *common.Sample // samples of mutation rate (numItems per flush interval)
	lastCheckTime int64          // last time when checking whether writers need adjustment

	// logging
	numExpand int // number of expansion
	numReduce int // number of reduction

	// throttling
	minimumDrainRate float64 // minimum drain rate after adding/removing writer
	saturateCount    int     // number of misses on meeting minimum drain rate

	// config
	enableWriterTuning bool    // enable tuning on writers
	adjustInterval     uint64  // interval to check whether writer need tuning
	samplingWindow     uint64  // sampling window
	samplingInterval   uint64  // sampling interval
	snapInterval       uint64  // snapshot interval
	scalingFactor      float64 // scaling factor for percentage increase on drain rate
	threshold          int     // threshold on number of misses on drain rate

	writerLock    sync.Mutex // mutex for writer tuning
	samplerStopCh chan bool  // stop sampler
	token         *token     // token
}

func newPlasmaSlice(path string, sliceId SliceId, idxDefn common.IndexDefn,
	idxInstId common.IndexInstId, partitionId common.PartitionId,
	isPrimary bool, numPartitions int,
	sysconf common.Config, idxStats *IndexStats, indexerStats *IndexerStats) (*plasmaSlice, error) {

	slice := &plasmaSlice{}

	_, err := os.Stat(path)
	if err != nil {
		os.Mkdir(path, 0777)
		slice.newBorn = true
	}

	slice.idxStats = idxStats
	slice.indexerStats = indexerStats

	slice.get_bytes = 0
	slice.insert_bytes = 0
	slice.delete_bytes = 0
	slice.flushedCount = 0
	slice.committedCount = 0
	slice.sysconf = sysconf
	slice.path = path
	slice.idxInstId = idxInstId
	slice.idxDefnId = idxDefn.DefnId
	slice.idxPartnId = partitionId
	slice.idxDefn = idxDefn
	slice.id = sliceId
	slice.maxNumWriters = sysconf["numSliceWriters"].Int()
	slice.hasPersistence = !sysconf["plasma.disablePersistence"].Bool()

	slice.maxRollbacks = sysconf["settings.plasma.recovery.max_rollbacks"].Int()

	updatePlasmaConfig(sysconf)
	if sysconf["plasma.UseQuotaTuner"].Bool() {
		go plasma.RunMemQuotaTuner()
	}

	numReaders := slice.sysconf["plasma.numReaders"].Int()
	slice.readers = make(chan *plasma.Reader, numReaders)

	slice.isPrimary = isPrimary
	slice.numPartitions = numPartitions

	slice.samplingWindow = uint64(sysconf["plasma.writer.tuning.sampling.window"].Int()) * uint64(time.Millisecond)
	slice.enableWriterTuning = sysconf["plasma.writer.tuning.enable"].Bool()
	slice.adjustInterval = uint64(sysconf["plasma.writer.tuning.adjust.interval"].Int()) * uint64(time.Millisecond)
	slice.samplingInterval = uint64(sysconf["plasma.writer.tuning.sampling.interval"].Int()) * uint64(time.Millisecond)
	slice.scalingFactor = sysconf["plasma.writer.tuning.throughput.scalingFactor"].Float64()
	slice.threshold = sysconf["plasma.writer.tuning.throttling.threshold"].Int()
	slice.drainRate = common.NewSample(int(slice.samplingWindow / slice.samplingInterval))
	slice.mutationRate = common.NewSample(int(slice.samplingWindow / slice.samplingInterval))
	slice.samplerStopCh = make(chan bool)
	slice.snapInterval = sysconf["settings.inmemory_snapshot.moi.interval"].Uint64() * uint64(time.Millisecond)

	if err := slice.initStores(); err != nil {
		// Index is unusable. Remove the data files and reinit
		if err == errStorageCorrupted {
			logging.Errorf("plasmaSlice:NewplasmaSlice Id %v IndexInstId %v PartitionId %v "+
				"fatal error occured: %v", sliceId, idxInstId, partitionId, err)
		}
		return nil, err
	}

	// Array related initialization
	_, slice.isArrayDistinct, slice.arrayExprPosition, err = queryutil.GetArrayExpressionPosition(idxDefn.SecExprs)
	if err != nil {
		return nil, err
	}

	// intiialize and start the writers
	slice.setupWriters()

	logging.Infof("plasmaSlice:NewplasmaSlice Created New Slice Id %v IndexInstId %v partitionId %v "+
		"WriterThreads %v cleaner %v", sliceId, idxInstId, partitionId, slice.numWriters, slice.mainstore.LSSCleanerConcurrency)

	slice.setCommittedCount()
	return slice, nil
}

func (slice *plasmaSlice) initStores() error {
	var err error
	cfg := plasma.DefaultConfig()
	cfg.UseMemoryMgmt = slice.sysconf["plasma.useMemMgmt"].Bool()
	cfg.FlushBufferSize = int(slice.sysconf["plasma.flushBufferSize"].Int())
	cfg.LSSLogSegmentSize = int64(slice.sysconf["plasma.LSSSegmentFileSize"].Int())
	cfg.UseCompression = slice.sysconf["plasma.useCompression"].Bool()
	cfg.AutoSwapper = true
	cfg.NumEvictorThreads = int(float32(runtime.NumCPU())*
		float32(slice.sysconf["plasma.evictionCPUPercent"].Int())/(100) + 0.5)
	cfg.DisableReadCaching = slice.sysconf["plasma.disableReadCaching"].Bool()
	cfg.AutoMVCCPurging = slice.sysconf["plasma.purger.enabled"].Bool()
	cfg.PurgerInterval = time.Duration(slice.sysconf["plasma.purger.interval"].Int()) * time.Second
	cfg.PurgeThreshold = slice.sysconf["plasma.purger.highThreshold"].Float64()
	cfg.PurgeLowThreshold = slice.sysconf["plasma.purger.lowThreshold"].Float64()
	cfg.PurgeCompactRatio = slice.sysconf["plasma.purger.compactRatio"].Float64()
	cfg.EnablePageChecksum = slice.sysconf["plasma.enablePageChecksum"].Bool()
	cfg.EnableLSSPageSMO = slice.sysconf["plasma.enableLSSPageSMO"].Bool()
	cfg.LSSReadAheadSize = int64(slice.sysconf["plasma.logReadAheadSize"].Int())
	cfg.CheckpointInterval = time.Second * time.Duration(slice.sysconf["plasma.checkpointInterval"].Int())
	cfg.LSSCleanerConcurrency = slice.sysconf["plasma.LSSCleanerConcurrency"].Int()
	cfg.AutoTuneLSSCleaning = slice.sysconf["plasma.AutoTuneLSSCleaner"].Bool()
	cfg.Compression = slice.sysconf["plasma.compression"].String()
	cfg.MaxPageSize = slice.sysconf["plasma.MaxPageSize"].Int()
	cfg.AutoLSSCleaning = !slice.sysconf["settings.compaction.plasma.manual"].Bool()

	if slice.numPartitions != 1 {
		cfg.LSSCleanerConcurrency = 1
	}

	var mode plasma.IOMode

	if slice.sysconf["plasma.useMmapReads"].Bool() {
		mode = plasma.MMapIO
	} else if slice.sysconf["plasma.useDirectIO"].Bool() {
		mode = plasma.DirectIO
	}

	cfg.IOMode = mode

	var mCfg, bCfg plasma.Config

	mCfg = cfg
	bCfg = cfg

	mCfg.MaxDeltaChainLen = slice.sysconf["plasma.mainIndex.maxNumPageDeltas"].Int()
	mCfg.MaxPageItems = slice.sysconf["plasma.mainIndex.pageSplitThreshold"].Int()
	mCfg.MinPageItems = slice.sysconf["plasma.mainIndex.pageMergeThreshold"].Int()
	mCfg.MaxPageLSSSegments = slice.sysconf["plasma.mainIndex.maxLSSPageSegments"].Int()
	mCfg.LSSCleanerThreshold = slice.sysconf["plasma.mainIndex.LSSFragmentation"].Int()
	mCfg.LSSCleanerMaxThreshold = slice.sysconf["plasma.mainIndex.maxLSSFragmentation"].Int()
	mCfg.LogPrefix = fmt.Sprintf("%s/%s/Mainstore#%d:%d ", slice.idxDefn.Bucket, slice.idxDefn.Name, slice.idxInstId, slice.idxPartnId)

	bCfg.MaxDeltaChainLen = slice.sysconf["plasma.backIndex.maxNumPageDeltas"].Int()
	bCfg.MaxPageItems = slice.sysconf["plasma.backIndex.pageSplitThreshold"].Int()
	bCfg.MinPageItems = slice.sysconf["plasma.backIndex.pageMergeThreshold"].Int()
	bCfg.MaxPageLSSSegments = slice.sysconf["plasma.backIndex.maxLSSPageSegments"].Int()
	bCfg.LSSCleanerThreshold = slice.sysconf["plasma.backIndex.LSSFragmentation"].Int()
	bCfg.LSSCleanerMaxThreshold = slice.sysconf["plasma.backIndex.maxLSSFragmentation"].Int()
	bCfg.LogPrefix = fmt.Sprintf("%s/%s/Backstore#%d:%d ", slice.idxDefn.Bucket, slice.idxDefn.Name, slice.idxInstId, slice.idxPartnId)

	if slice.hasPersistence {
		mCfg.File = filepath.Join(slice.path, "mainIndex")
		bCfg.File = filepath.Join(slice.path, "docIndex")
	}

	var wg sync.WaitGroup
	var mErr, bErr error
	t0 := time.Now()

	// Recover mainindex
	wg.Add(1)
	go func() {
		defer wg.Done()

		slice.mainstore, mErr = plasma.New(mCfg)
		if mErr != nil {
			mErr = fmt.Errorf("Unable to initialize %s, err = %v", mCfg.File, mErr)
			return
		}
	}()

	if !slice.isPrimary {
		// Recover backindex
		wg.Add(1)
		go func() {
			defer wg.Done()
			slice.backstore, bErr = plasma.New(bCfg)
			if bErr != nil {
				bErr = fmt.Errorf("Unable to initialize %s, err = %v", bCfg.File, bErr)
				return
			}
		}()
	}

	wg.Wait()

	// In case of errors, close the opened stores
	if mErr != nil {
		if !slice.isPrimary && bErr == nil {
			slice.backstore.Close()
		}
	} else if bErr != nil {
		if mErr == nil {
			slice.mainstore.Close()
		}
	}

	// Return fatal error with higher priority.
	if mErr != nil && plasma.IsFatalError(mErr) {
		logging.Errorf("plasmaSlice:NewplasmaSlice Id %v IndexInstId %v "+
			"fatal error occured: %v", slice.Id, slice.idxInstId, mErr)
		return errStorageCorrupted
	}

	if bErr != nil && plasma.IsFatalError(bErr) {
		logging.Errorf("plasmaSlice:NewplasmaSlice Id %v IndexInstId %v "+
			"fatal error occured: %v", slice.Id, slice.idxInstId, bErr)
		return errStorageCorrupted
	}

	// If both mErr and bErr are not fatal, return mErr with higher priority
	if mErr != nil {
		return mErr
	}

	if bErr != nil {
		return bErr
	}

	for i := 0; i < cap(slice.readers); i++ {
		slice.readers <- slice.mainstore.NewReader()
	}

	if !slice.newBorn {
		logging.Infof("plasmaSlice::doRecovery SliceId %v IndexInstId %v PartitionId %v Recovering from recovery point ..",
			slice.id, slice.idxInstId, slice.idxPartnId)
		err = slice.doRecovery()
		dur := time.Since(t0)
		if err == nil {
			slice.idxStats.diskSnapLoadDuration.Set(int64(dur / time.Millisecond))
			logging.Infof("plasmaSlice::doRecovery SliceId %v IndexInstId %v PartitionId %v Warmup took %v",
				slice.id, slice.idxInstId, slice.idxPartnId, dur)
		}
	}

	return err
}

type plasmaReaderCtx struct {
	ch chan *plasma.Reader
	r  *plasma.Reader
	cursorCtx
}

func (ctx *plasmaReaderCtx) Init() {
	ctx.r = <-ctx.ch
}

func (ctx *plasmaReaderCtx) Done() {
	if ctx.r != nil {
		ctx.ch <- ctx.r
	}
}

func (mdb *plasmaSlice) GetReaderContext() IndexReaderContext {
	return &plasmaReaderCtx{
		ch: mdb.readers,
	}
}

func cmpRPMeta(a, b []byte) int {
	av := binary.BigEndian.Uint64(a[:8])
	bv := binary.BigEndian.Uint64(b[:8])
	return int(av - bv)
}

func (mdb *plasmaSlice) doRecovery() error {
	snaps, err := mdb.GetSnapshots()
	if err != nil {
		return err
	}

	if len(snaps) == 0 {
		logging.Infof("plasmaSlice::doRecovery SliceId %v IndexInstId %v PartitionId %v Unable to find recovery point. Resetting store ..",
			mdb.id, mdb.idxInstId, mdb.idxPartnId)
		mdb.resetStores()
	} else {
		err := mdb.restore(snaps[0])
		return err
	}

	return nil
}

func (mdb *plasmaSlice) IncrRef() {
	mdb.lock.Lock()
	defer mdb.lock.Unlock()

	mdb.refCount++
}

func (mdb *plasmaSlice) DecrRef() {
	mdb.lock.Lock()
	defer mdb.lock.Unlock()

	mdb.refCount--
	if mdb.refCount == 0 {
		if mdb.isSoftClosed {
			tryCloseplasmaSlice(mdb)
		}
		if mdb.isSoftDeleted {
			tryDeleteplasmaSlice(mdb)
		}
	}
}

func (mdb *plasmaSlice) Insert(key []byte, docid []byte, meta *MutationMeta) error {
	op := opUpdate
	if meta.firstSnap {
		op = opInsert
	}

	mut := indexMutation{
		op:    op,
		key:   key,
		docid: docid,
		meta:  meta,
	}

	atomic.AddInt64(&mdb.qCount, 1)
	mdb.cmdCh[int(meta.vbucket)%mdb.numWriters] <- mut
	mdb.idxStats.numDocsFlushQueued.Add(1)
	return mdb.fatalDbErr
}

func (mdb *plasmaSlice) Delete(docid []byte, meta *MutationMeta) error {
	if !meta.firstSnap {
		atomic.AddInt64(&mdb.qCount, 1)
		mdb.idxStats.numDocsFlushQueued.Add(1)
		mdb.cmdCh[int(meta.vbucket)%mdb.numWriters] <- indexMutation{op: opDelete, docid: docid}
	}
	return mdb.fatalDbErr
}

func (mdb *plasmaSlice) handleCommandsWorker(workerId int) {
	var start time.Time
	var elapsed time.Duration
	var icmd indexMutation

loop:
	for {
		var nmut int
		select {
		case icmd = <-mdb.cmdCh[workerId]:
			switch icmd.op {
			case opUpdate, opInsert:
				start = time.Now()
				nmut = mdb.insert(icmd.key, icmd.docid, workerId, icmd.op == opInsert, icmd.meta)
				elapsed = time.Since(start)
				mdb.totalFlushTime += elapsed

			case opDelete:
				start = time.Now()
				nmut = mdb.delete(icmd.docid, workerId)
				elapsed = time.Since(start)
				mdb.totalFlushTime += elapsed

			default:
				logging.Errorf("plasmaSlice::handleCommandsWorker \n\tSliceId %v IndexInstId %v PartitionId %v Received "+
					"Unknown Command %v", mdb.id, mdb.idxInstId, mdb.idxPartnId, logging.TagUD(icmd))
			}

			mdb.idxStats.numItemsFlushed.Add(int64(nmut))
			mdb.idxStats.numDocsIndexed.Add(1)
			atomic.AddInt64(&mdb.qCount, -1)

			if mdb.enableWriterTuning {
				atomic.AddInt64(&mdb.drainTime, elapsed.Nanoseconds())
				atomic.AddInt64(&mdb.numItems, int64(nmut))
			}

		case _, ok := <-mdb.stopCh[workerId]:
			if ok {
				mdb.stopCh[workerId] <- true
			}
			break loop

		case <-mdb.workerDone[workerId]:
			mdb.workerDone[workerId] <- true

		}
	}
}

func (mdb *plasmaSlice) insert(key []byte, docid []byte, workerId int,
	init bool, meta *MutationMeta) int {
	var nmut int

	if mdb.isPrimary {
		nmut = mdb.insertPrimaryIndex(key, docid, workerId)
	} else if len(key) == 0 {
		nmut = mdb.delete(docid, workerId)
	} else {
		if mdb.idxDefn.IsArrayIndex {
			nmut = mdb.insertSecArrayIndex(key, docid, workerId, init, meta)
		} else {
			nmut = mdb.insertSecIndex(key, docid, workerId, init, meta)
		}
	}

	mdb.logWriterStat()
	return nmut
}

func (mdb *plasmaSlice) insertPrimaryIndex(key []byte, docid []byte, workerId int) int {

	entry, err := NewPrimaryIndexEntry(docid)
	common.CrashOnError(err)

	mdb.main[workerId].Begin()
	defer mdb.main[workerId].End()

	_, err = mdb.main[workerId].LookupKV(entry)
	if err == plasma.ErrItemNotFound {
		t0 := time.Now()
		mdb.main[workerId].InsertKV(entry, nil)
		mdb.idxStats.Timings.stKVSet.Put(time.Now().Sub(t0))
		atomic.AddInt64(&mdb.insert_bytes, int64(len(entry)))
		mdb.isDirty = true
		return 1
	}

	return 0
}

func (mdb *plasmaSlice) insertSecIndex(key []byte, docid []byte, workerId int, init bool, meta *MutationMeta) int {
	t0 := time.Now()

	var ndel int
	var changed bool

	// The docid does not exist if the doc is initialized for the first time
	if !init {
		if ndel, changed = mdb.deleteSecIndex(docid, key, workerId); !changed {
			return 0
		}
	}

	mdb.encodeBuf[workerId] = resizeEncodeBuf(mdb.encodeBuf[workerId], len(key), allowLargeKeys)
	entry, err := NewSecondaryIndexEntry(key, docid, mdb.idxDefn.IsArrayIndex,
		1, mdb.idxDefn.Desc, mdb.encodeBuf[workerId], meta)
	if err != nil {
		logging.Errorf("plasmaSlice::insertSecIndex Slice Id %v IndexInstId %v PartitionId %v "+
			"Skipping docid:%s (%v)", mdb.Id, mdb.idxInstId, mdb.idxPartnId, logging.TagStrUD(docid), err)
		return ndel
	}

	if len(key) > 0 {
		mdb.main[workerId].Begin()
		defer mdb.main[workerId].End()
		mdb.back[workerId].Begin()
		defer mdb.back[workerId].End()

		mdb.main[workerId].InsertKV(entry, nil)
		// entry2BackEntry overwrites the buffer to remove docid
		backEntry := entry2BackEntry(entry)
		mdb.back[workerId].InsertKV(docid, backEntry)

		mdb.idxStats.Timings.stKVSet.Put(time.Now().Sub(t0))
		atomic.AddInt64(&mdb.insert_bytes, int64(len(docid)+len(entry)))
	}

	mdb.isDirty = true
	return 1
}

func (mdb *plasmaSlice) insertSecArrayIndex(key []byte, docid []byte, workerId int,
	init bool, meta *MutationMeta) (nmut int) {
	var err error
	var oldkey []byte

	mdb.arrayBuf2[workerId] = resizeArrayBuf(mdb.arrayBuf2[workerId], 3*len(key))

	if !allowLargeKeys && len(key) > maxArrayIndexEntrySize {
		logging.Errorf("plasmaSlice::insertSecArrayIndex Error indexing docid: %s in Slice: %v. Error: Encoded array key (size %v) too long (> %v). Skipped.",
			logging.TagStrUD(docid), mdb.id, len(key), maxArrayIndexEntrySize)
		mdb.deleteSecArrayIndex(docid, workerId)
		return 0
	}

	mdb.main[workerId].Begin()
	defer mdb.main[workerId].End()
	mdb.back[workerId].Begin()
	defer mdb.back[workerId].End()

	// The docid does not exist if the doc is initialized for the first time
	if !init {
		oldkey, err = mdb.back[workerId].LookupKV(docid)
		if err == plasma.ErrItemNotFound {
			oldkey = nil
		}
	}

	var oldEntriesBytes, newEntriesBytes [][]byte
	var oldKeyCount, newKeyCount []int
	var newbufLen int
	if oldkey != nil {
		if bytes.Equal(oldkey, key) {
			return
		}

		var tmpBuf []byte
		if len(oldkey)*3 > cap(mdb.arrayBuf1[workerId]) {
			tmpBuf = make([]byte, 0, len(oldkey)*3)
		} else {
			tmpBuf = mdb.arrayBuf1[workerId]
		}

		//get the key in original form
		if mdb.idxDefn.Desc != nil {
			jsonEncoder.ReverseCollate(oldkey, mdb.idxDefn.Desc)
		}

		oldEntriesBytes, oldKeyCount, newbufLen, err = ArrayIndexItems(oldkey, mdb.arrayExprPosition,
			tmpBuf, mdb.isArrayDistinct, false)
		mdb.arrayBuf1[workerId] = resizeArrayBuf(mdb.arrayBuf1[workerId], newbufLen)

		if err != nil {
			logging.Errorf("plasmaSlice::insertSecArrayIndex SliceId %v IndexInstId %v PartitionId %v Error in retrieving "+
				"compostite old secondary keys. Skipping docid:%s Error: %v",
				mdb.id, mdb.idxInstId, mdb.idxPartnId, logging.TagStrUD(docid), err)
			mdb.deleteSecArrayIndex(docid, workerId)
			return 0
		}
	}

	if key != nil {

		newEntriesBytes, newKeyCount, newbufLen, err = ArrayIndexItems(key, mdb.arrayExprPosition,
			mdb.arrayBuf2[workerId], mdb.isArrayDistinct, !allowLargeKeys)
		mdb.arrayBuf2[workerId] = resizeArrayBuf(mdb.arrayBuf2[workerId], newbufLen)
		if err != nil {
			logging.Errorf("plasmaSlice::insertSecArrayIndex SliceId %v IndexInstId %v PartitionId %v Error in creating "+
				"compostite new secondary keys. Skipping docid:%s Error: %v",
				mdb.id, mdb.idxInstId, mdb.idxPartnId, logging.TagStrUD(docid), err)
			mdb.deleteSecArrayIndex(docid, workerId)
			return 0
		}
	}

	var indexEntriesToBeAdded, indexEntriesToBeDeleted [][]byte
	if len(oldEntriesBytes) == 0 { // It is a new key. Nothing to delete
		indexEntriesToBeDeleted = nil
		indexEntriesToBeAdded = newEntriesBytes
	} else if len(newEntriesBytes) == 0 { // New key is nil. Nothing to add
		indexEntriesToBeAdded = nil
		indexEntriesToBeDeleted = oldEntriesBytes
	} else {
		indexEntriesToBeAdded, indexEntriesToBeDeleted = CompareArrayEntriesWithCount(newEntriesBytes, oldEntriesBytes, newKeyCount, oldKeyCount)
	}

	nmut = 0

	rollbackDeletes := func(upto int) {
		for i := 0; i <= upto; i++ {
			item := indexEntriesToBeDeleted[i]
			if item != nil { // nil item indicates it should be ignored
				entry, err := NewSecondaryIndexEntry(item, docid, false,
					oldKeyCount[i], mdb.idxDefn.Desc, mdb.encodeBuf[workerId][:0], nil)
				common.CrashOnError(err)
				// Add back
				mdb.main[workerId].InsertKV(entry, nil)
			}
		}
	}

	rollbackAdds := func(upto int) {
		for i := 0; i <= upto; i++ {
			key := indexEntriesToBeAdded[i]
			if key != nil { // nil item indicates it should be ignored
				entry, err := NewSecondaryIndexEntry(key, docid, false,
					newKeyCount[i], mdb.idxDefn.Desc, mdb.encodeBuf[workerId][:0], meta)
				common.CrashOnError(err)
				// Delete back
				mdb.main[workerId].DeleteKV(entry)
			}
		}
	}

	// Delete each of indexEntriesToBeDeleted from main index
	for i, item := range indexEntriesToBeDeleted {
		if item != nil { // nil item indicates it should not be deleted
			var keyToBeDeleted []byte
			mdb.encodeBuf[workerId] = resizeEncodeBuf(mdb.encodeBuf[workerId], len(item), true)
			if keyToBeDeleted, err = GetIndexEntryBytes3(item, docid, false, false,
				oldKeyCount[i], mdb.idxDefn.Desc, mdb.encodeBuf[workerId], nil); err != nil {
				rollbackDeletes(i - 1)
				logging.Errorf("plasmaSlice::insertSecArrayIndex SliceId %v IndexInstId %v PartitionId %v Error forming entry "+
					"to be added to main index. Skipping docid:%s Error: %v",
					mdb.id, mdb.idxInstId, mdb.idxPartnId, logging.TagStrUD(docid), err)
				mdb.deleteSecArrayIndex(docid, workerId)
				return 0
			}
			t0 := time.Now()
			mdb.main[workerId].DeleteKV(keyToBeDeleted)
			mdb.idxStats.Timings.stKVDelete.Put(time.Now().Sub(t0))
			atomic.AddInt64(&mdb.delete_bytes, int64(len(keyToBeDeleted)))
			nmut++
		}
	}

	// Insert each of indexEntriesToBeAdded into main index
	for i, item := range indexEntriesToBeAdded {
		if item != nil { // nil item indicates it should not be added
			var keyToBeAdded []byte
			mdb.encodeBuf[workerId] = resizeEncodeBuf(mdb.encodeBuf[workerId], len(item), allowLargeKeys)
			if keyToBeAdded, err = GetIndexEntryBytes2(item, docid, false, false,
				newKeyCount[i], mdb.idxDefn.Desc, mdb.encodeBuf[workerId], meta); err != nil {
				rollbackDeletes(len(indexEntriesToBeDeleted) - 1)
				rollbackAdds(i - 1)
				logging.Errorf("plasmaSlice::insertSecArrayIndex SliceId %v IndexInstId %v PartitionId %v Error forming entry "+
					"to be added to main index. Skipping docid:%s Error: %v",
					mdb.id, mdb.idxInstId, mdb.idxPartnId, logging.TagStrUD(docid), err)
				mdb.deleteSecArrayIndex(docid, workerId)
				return 0
			}
			t0 := time.Now()
			mdb.main[workerId].InsertKV(keyToBeAdded, nil)
			mdb.idxStats.Timings.stKVSet.Put(time.Now().Sub(t0))
			atomic.AddInt64(&mdb.insert_bytes, int64(len(keyToBeAdded)))
			nmut++
		}
	}

	// If a field value changed from "existing" to "missing" (ie, key = nil),
	// we need to remove back index entry corresponding to the previous "existing" value.
	if key == nil {
		if oldkey != nil {
			t0 := time.Now()
			mdb.back[workerId].DeleteKV(docid)
			mdb.idxStats.Timings.stKVDelete.Put(time.Now().Sub(t0))
			atomic.AddInt64(&mdb.delete_bytes, int64(len(docid)))
		}
	} else { //set the back index entry <docid, encodedkey>

		//convert to storage format
		if mdb.idxDefn.Desc != nil {
			jsonEncoder.ReverseCollate(key, mdb.idxDefn.Desc)
		}

		if oldkey != nil {
			t0 := time.Now()
			mdb.back[workerId].DeleteKV(docid)
			mdb.idxStats.Timings.stKVDelete.Put(time.Now().Sub(t0))
			atomic.AddInt64(&mdb.delete_bytes, int64(len(docid)))
		}

		t0 := time.Now()
		mdb.back[workerId].InsertKV(docid, key)
		mdb.idxStats.Timings.stKVSet.Put(time.Now().Sub(t0))
		atomic.AddInt64(&mdb.insert_bytes, int64(len(docid)+len(key)))
	}

	mdb.isDirty = true
	return nmut
}

func (mdb *plasmaSlice) delete(docid []byte, workerId int) int {
	var nmut int

	if mdb.isPrimary {
		nmut = mdb.deletePrimaryIndex(docid, workerId)
	} else if !mdb.idxDefn.IsArrayIndex {
		nmut, _ = mdb.deleteSecIndex(docid, nil, workerId)
	} else {
		nmut = mdb.deleteSecArrayIndex(docid, workerId)
	}

	mdb.logWriterStat()
	return nmut
}

func (mdb *plasmaSlice) deletePrimaryIndex(docid []byte, workerId int) (nmut int) {
	if docid == nil {
		common.CrashOnError(errors.New("Nil Primary Key"))
		return
	}

	// docid -> key format
	entry, err := NewPrimaryIndexEntry(docid)
	common.CrashOnError(err)

	// Delete from main index
	t0 := time.Now()
	itm := entry.Bytes()

	mdb.main[workerId].Begin()
	defer mdb.main[workerId].End()

	if _, err := mdb.main[workerId].LookupKV(entry); err == plasma.ErrItemNoValue {
		mdb.main[workerId].DeleteKV(itm)
		mdb.idxStats.Timings.stKVDelete.Put(time.Now().Sub(t0))
		atomic.AddInt64(&mdb.delete_bytes, int64(len(entry.Bytes())))
		mdb.isDirty = true
		return 1
	}

	return 0
}

func (mdb *plasmaSlice) deleteSecIndex(docid []byte, compareKey []byte, workerId int) (ndel int, changed bool) {
	// Delete entry from back and main index if present
	mdb.back[workerId].Begin()
	defer mdb.back[workerId].End()

	backEntry, err := mdb.back[workerId].LookupKV(docid)
	mdb.encodeBuf[workerId] = resizeEncodeBuf(mdb.encodeBuf[workerId], len(backEntry), true)
	buf := mdb.encodeBuf[workerId]

	if err == nil {
		// Delete the entries only if the entry is different
		if hasEqualBackEntry(compareKey, backEntry) {
			return 0, false
		}

		t0 := time.Now()
		atomic.AddInt64(&mdb.delete_bytes, int64(len(docid)))
		mdb.main[workerId].Begin()
		defer mdb.main[workerId].End()
		mdb.back[workerId].DeleteKV(docid)
		entry := backEntry2entry(docid, backEntry, buf)
		mdb.main[workerId].DeleteKV(entry)
		mdb.idxStats.Timings.stKVDelete.Put(time.Since(t0))
	}

	mdb.isDirty = true
	return 1, true
}

func (mdb *plasmaSlice) deleteSecArrayIndex(docid []byte, workerId int) (nmut int) {
	var olditm []byte
	var err error

	mdb.back[workerId].Begin()
	defer mdb.back[workerId].End()
	olditm, err = mdb.back[workerId].LookupKV(docid)
	if err == plasma.ErrItemNotFound {
		olditm = nil
	}

	if olditm == nil {
		return
	}

	var tmpBuf []byte
	if len(olditm)*3 > cap(mdb.arrayBuf1[workerId]) {
		tmpBuf = make([]byte, 0, len(olditm)*3)
	} else {
		tmpBuf = mdb.arrayBuf1[workerId]
	}

	//get the key in original form
	if mdb.idxDefn.Desc != nil {
		jsonEncoder.ReverseCollate(olditm, mdb.idxDefn.Desc)
	}

	indexEntriesToBeDeleted, keyCount, _, err := ArrayIndexItems(olditm, mdb.arrayExprPosition,
		tmpBuf, mdb.isArrayDistinct, false)
	if err != nil {
		// TODO: Do not crash for non-storage operation. Force delete the old entries
		common.CrashOnError(err)
		logging.Errorf("plasmaSlice::deleteSecArrayIndex \n\tSliceId %v IndexInstId %v PartitionId %v Error in retrieving "+
			"compostite old secondary keys %v", mdb.id, mdb.idxInstId, mdb.idxPartnId, err)
		return
	}

	mdb.main[workerId].Begin()
	defer mdb.main[workerId].End()

	var t0 time.Time
	// Delete each of indexEntriesToBeDeleted from main index
	for i, item := range indexEntriesToBeDeleted {
		var keyToBeDeleted []byte
		var tmpBuf []byte
		tmpBuf = resizeEncodeBuf(mdb.encodeBuf[workerId], len(item), true)
		// TODO: Use method that skips size check for bug MB-22183
		if keyToBeDeleted, err = GetIndexEntryBytes3(item, docid, false, false, keyCount[i],
			mdb.idxDefn.Desc, tmpBuf, nil); err != nil {
			common.CrashOnError(err)
			logging.Errorf("plasmaSlice::deleteSecArrayIndex \n\tSliceId %v IndexInstId %v PartitionId %v Error from GetIndexEntryBytes2 "+
				"for entry to be deleted from main index %v", mdb.id, mdb.idxInstId, mdb.idxPartnId, err)
			return
		}
		t0 := time.Now()
		mdb.main[workerId].DeleteKV(keyToBeDeleted)
		mdb.idxStats.Timings.stKVDelete.Put(time.Now().Sub(t0))
		atomic.AddInt64(&mdb.delete_bytes, int64(len(keyToBeDeleted)))
	}

	//delete from the back index
	t0 = time.Now()
	mdb.back[workerId].DeleteKV(docid)
	mdb.idxStats.Timings.stKVDelete.Put(time.Now().Sub(t0))
	atomic.AddInt64(&mdb.delete_bytes, int64(len(docid)))
	mdb.isDirty = true
	return len(indexEntriesToBeDeleted)
}

//checkFatalDbError checks if the error returned from DB
//is fatal and stores it. This error will be returned
//to caller on next DB operation
func (mdb *plasmaSlice) checkFatalDbError(err error) {

	//panic on all DB errors and recover rather than risk
	//inconsistent db state
	common.CrashOnError(err)

	errStr := err.Error()
	switch errStr {

	case "checksum error", "file corruption", "no db instance",
		"alloc fail", "seek fail", "fsync fail":
		mdb.fatalDbErr = err

	}

}

type plasmaSnapshotInfo struct {
	Ts        *common.TsVbuuid
	Committed bool
	Count     int64

	mRP, bRP *plasma.RecoveryPoint
}

type plasmaSnapshot struct {
	slice      *plasmaSlice
	idxDefnId  common.IndexDefnId
	idxInstId  common.IndexInstId
	idxPartnId common.PartitionId
	ts         *common.TsVbuuid
	info       SnapshotInfo

	MainSnap *plasma.Snapshot
	BackSnap *plasma.Snapshot

	committed bool

	refCount int32
}

// Creates an open snapshot handle from snapshot info
// Snapshot info is obtained from NewSnapshot() or GetSnapshots() API
// Returns error if snapshot handle cannot be created.
func (mdb *plasmaSlice) OpenSnapshot(info SnapshotInfo) (Snapshot, error) {
	snapInfo := info.(*plasmaSnapshotInfo)

	s := &plasmaSnapshot{slice: mdb,
		idxDefnId:  mdb.idxDefnId,
		idxInstId:  mdb.idxInstId,
		idxPartnId: mdb.idxPartnId,
		info:       info,
		ts:         snapInfo.Timestamp(),
		committed:  info.IsCommitted(),
		MainSnap:   mdb.mainstore.NewSnapshot(),
	}

	if !mdb.isPrimary {
		s.BackSnap = mdb.backstore.NewSnapshot()
	}

	s.Open()
	s.slice.IncrRef()

	if s.committed && mdb.hasPersistence {
		mdb.doPersistSnapshot(s)
	}

	if info.IsCommitted() {
		logging.Infof("plasmaSlice::OpenSnapshot SliceId %v IndexInstId %v PartitionId %v Creating New "+
			"Snapshot %v", mdb.id, mdb.idxInstId, mdb.idxPartnId, snapInfo)
	}
	mdb.setCommittedCount()

	return s, nil
}

var plasmaPersistenceMutex sync.Mutex

func (mdb *plasmaSlice) doPersistSnapshot(s *plasmaSnapshot) {
	if atomic.CompareAndSwapInt32(&mdb.isPersistorActive, 0, 1) {
		s.MainSnap.Open()
		if !mdb.isPrimary {
			s.BackSnap.Open()
		}

		go func() {
			defer atomic.StoreInt32(&mdb.isPersistorActive, 0)

			logging.Infof("PlasmaSlice Slice Id %v, IndexInstId %v, PartitionId %v Creating recovery point ...", mdb.id, mdb.idxInstId, mdb.idxPartnId)
			t0 := time.Now()

			meta, err := json.Marshal(s.ts)
			common.CrashOnError(err)
			timeHdr := make([]byte, 8)
			binary.BigEndian.PutUint64(timeHdr, uint64(time.Now().UnixNano()))
			meta = append(timeHdr, meta...)

			// To prevent persistence from eating up all the disk bandwidth
			// and slowing down query, we wish to ensure that only 1 instance
			// gets persisted at once across all instances on this node.
			// Since both main and back snapshots are open, we wish to ensure
			// that serialization of the main and back index persistence happens
			// only via this callback to ensure that neither of these snapshots
			// are held open until the other completes recovery point creation.
			tokenCh := make(chan bool, 1) // To locally serialize main & back
			tokenCh <- true
			serializePersistence := func(s *plasma.Plasma) error {
				<-tokenCh
				plasmaPersistenceMutex.Lock()
				return nil
			}

			var concurr int = int(float32(runtime.NumCPU())*float32(mdb.sysconf["plasma.persistenceCPUPercent"].Int())/(100*2) + 0.75)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				mdb.mainstore.CreateRecoveryPoint(s.MainSnap, meta,
					concurr, serializePersistence)
				tokenCh <- true
				plasmaPersistenceMutex.Unlock()
				wg.Done()
			}()

			if !mdb.isPrimary {
				mdb.backstore.CreateRecoveryPoint(s.BackSnap, meta, concurr,
					serializePersistence)
				tokenCh <- true
				plasmaPersistenceMutex.Unlock()
			}
			wg.Wait()

			dur := time.Since(t0)
			logging.Infof("PlasmaSlice Slice Id %v, IndexInstId %v, PartitionId %v Created recovery point (took %v)",
				mdb.id, mdb.idxInstId, mdb.idxPartnId, dur)
			mdb.idxStats.diskSnapStoreDuration.Set(int64(dur / time.Millisecond))

			// Cleanup old recovery points
			mRPs := mdb.mainstore.GetRecoveryPoints()
			if len(mRPs) > mdb.maxRollbacks {
				for i := 0; i < len(mRPs)-mdb.maxRollbacks; i++ {
					mdb.mainstore.RemoveRecoveryPoint(mRPs[i])
				}
			}

			if !mdb.isPrimary {
				bRPs := mdb.backstore.GetRecoveryPoints()
				if len(bRPs) > mdb.maxRollbacks {
					for i := 0; i < len(bRPs)-mdb.maxRollbacks; i++ {
						mdb.backstore.RemoveRecoveryPoint(bRPs[i])
					}
				}
			}
		}()
	} else {
		logging.Infof("PlasmaSlice Slice Id %v, IndexInstId %v, PartitionId %v Skipping ondisk"+
			" snapshot. A snapshot writer is in progress.", mdb.id, mdb.idxInstId, mdb.idxPartnId)
	}
}

func (mdb *plasmaSlice) GetSnapshots() ([]SnapshotInfo, error) {
	var mRPs, bRPs []*plasma.RecoveryPoint
	var minRP, maxRP []byte

	getRPs := func(rpts []*plasma.RecoveryPoint) []*plasma.RecoveryPoint {
		var newRpts []*plasma.RecoveryPoint
		for _, rp := range rpts {
			if cmpRPMeta(rp.Meta(), minRP) < 0 {
				continue
			}

			if cmpRPMeta(rp.Meta(), maxRP) > 0 {
				break
			}

			newRpts = append(newRpts, rp)
		}

		return newRpts
	}

	// Find out the common recovery points between mainIndex and backIndex
	mRPs = mdb.mainstore.GetRecoveryPoints()
	if len(mRPs) > 0 {
		minRP = mRPs[0].Meta()
		maxRP = mRPs[len(mRPs)-1].Meta()
	}

	if !mdb.isPrimary {
		bRPs = mdb.backstore.GetRecoveryPoints()
		if len(bRPs) > 0 {
			if cmpRPMeta(bRPs[0].Meta(), minRP) > 0 {
				minRP = bRPs[0].Meta()
			}

			if cmpRPMeta(bRPs[len(bRPs)-1].Meta(), maxRP) < 0 {
				maxRP = bRPs[len(bRPs)-1].Meta()
			}
		}

		bRPs = getRPs(bRPs)
	}

	mRPs = getRPs(mRPs)

	if !mdb.isPrimary && len(mRPs) != len(bRPs) {
		return nil, nil
	}

	var infos []SnapshotInfo
	for i := len(mRPs) - 1; i >= 0; i-- {
		info := &plasmaSnapshotInfo{
			mRP:   mRPs[i],
			Count: mRPs[i].ItemsCount(),
		}

		if err := json.Unmarshal(info.mRP.Meta()[8:], &info.Ts); err != nil {
			return nil, fmt.Errorf("Unable to decode snapshot meta err %v", err)
		}

		if !mdb.isPrimary {
			info.bRP = bRPs[i]
		}

		infos = append(infos, info)
	}

	return infos, nil
}

func (mdb *plasmaSlice) setCommittedCount() {
	curr := mdb.mainstore.ItemsCount()
	atomic.StoreUint64(&mdb.committedCount, uint64(curr))
}

func (mdb *plasmaSlice) GetCommittedCount() uint64 {
	return atomic.LoadUint64(&mdb.committedCount)
}

func (mdb *plasmaSlice) resetStores() {
	// Clear all readers
	for i := 0; i < cap(mdb.readers); i++ {
		<-mdb.readers
	}

	numWriters := mdb.numWriters
	mdb.freeAllWriters()

	mdb.mainstore.Close()
	if !mdb.isPrimary {
		mdb.backstore.Close()
	}

	os.RemoveAll(mdb.path)
	mdb.newBorn = true
	mdb.initStores()
	mdb.startWriters(numWriters)
	mdb.setCommittedCount()
	mdb.idxStats.itemsCount.Set(0)
}

func (mdb *plasmaSlice) Rollback(o SnapshotInfo) error {
	mdb.waitPersist()
	mdb.waitForPersistorThread()
	qc := atomic.LoadInt64(&mdb.qCount)
	if qc > 0 {
		common.CrashOnError(errors.New("Slice Invariant Violation - rollback with pending mutations"))
	}

	// Block all scan requests
	var readers []*plasma.Reader
	for i := 0; i < cap(mdb.readers); i++ {
		readers = append(readers, <-mdb.readers)
	}

	err := mdb.restore(o)
	for i := 0; i < cap(mdb.readers); i++ {
		mdb.readers <- readers[i]
	}

	return err
}

func (mdb *plasmaSlice) restore(o SnapshotInfo) error {
	var wg sync.WaitGroup
	var mErr, bErr error
	info := o.(*plasmaSnapshotInfo)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var s *plasma.Snapshot
		if s, mErr = mdb.mainstore.Rollback(info.mRP); mErr == nil {
			s.Close()
		}
	}()

	if !mdb.isPrimary {

		wg.Add(1)
		go func() {
			defer wg.Done()
			var s *plasma.Snapshot
			if s, bErr = mdb.backstore.Rollback(info.bRP); bErr == nil {
				s.Close()
			}
		}()
	}

	wg.Wait()

	if mErr != nil || bErr != nil {
		return fmt.Errorf("Rollback error %v %v", mErr, bErr)
	}

	return nil
}

//RollbackToZero rollbacks the slice to initial state. Return error if
//not possible
func (mdb *plasmaSlice) RollbackToZero() error {
	mdb.waitPersist()
	mdb.waitForPersistorThread()

	mdb.resetStores()

	return nil
}

//slice insert/delete methods are async. There
//can be outstanding mutations in internal queue to flush even
//after insert/delete have return success to caller.
//This method provides a mechanism to wait till internal
//queue is empty.
func (mdb *plasmaSlice) waitPersist() {

	if !mdb.checkAllWorkersDone() {
		//every SLICE_COMMIT_POLL_INTERVAL milliseconds,
		//check for outstanding mutations. If there are
		//none, proceed with the commit.
		mdb.confLock.RLock()
		commitPollInterval := mdb.sysconf["storage.moi.commitPollInterval"].Uint64()
		mdb.confLock.RUnlock()

		for {
			if mdb.checkAllWorkersDone() {
				break
			}
			time.Sleep(time.Millisecond * time.Duration(commitPollInterval))
		}
	}

}

//Commit persists the outstanding writes in underlying
//forestdb database. If Commit returns error, slice
//should be rolled back to previous snapshot.
func (mdb *plasmaSlice) NewSnapshot(ts *common.TsVbuuid, commit bool) (SnapshotInfo, error) {

	mdb.waitPersist()

	qc := atomic.LoadInt64(&mdb.qCount)
	if qc > 0 {
		common.CrashOnError(errors.New("Slice Invariant Violation - commit with pending mutations"))
	}

	mdb.isDirty = false

	newSnapshotInfo := &plasmaSnapshotInfo{
		Ts:        ts,
		Committed: commit,
		Count:     mdb.mainstore.ItemsCount(),
	}

	return newSnapshotInfo, nil
}

func (mdb *plasmaSlice) FlushDone() {

	if !mdb.enableWriterTuning {
		return
	}

	mdb.waitPersist()

	qc := atomic.LoadInt64(&mdb.qCount)
	if qc > 0 {
		common.CrashOnError(errors.New("Slice Invariant Violation - commit with pending mutations"))
	}

	// Adjust the number of writers at inmemory snapshot or persisted snapshot
	mdb.adjustWriters()
}

//checkAllWorkersDone return true if all workers have
//finished processing
func (mdb *plasmaSlice) checkAllWorkersDone() bool {

	//if there are mutations in the cmdCh, workers are
	//not yet done
	if mdb.getCmdsCount() > 0 {
		return false
	}

	//worker queue is empty, make sure both workers are done
	//processing the last mutation
	for i := 0; i < mdb.numWriters; i++ {
		mdb.workerDone[i] <- true
		<-mdb.workerDone[i]
	}

	return true
}

func (mdb *plasmaSlice) Close() {
	mdb.lock.Lock()
	defer mdb.lock.Unlock()

	logging.Infof("plasmaSlice::Close Closing Slice Id %v, IndexInstId %v, PartitionId %v, "+
		"IndexDefnId %v", mdb.id, mdb.idxInstId, mdb.idxPartnId, mdb.idxDefnId)

	//signal shutdown for command handler routines
	mdb.cleanupWritersOnClose()

	if mdb.refCount > 0 {
		mdb.isSoftClosed = true
	} else {
		tryCloseplasmaSlice(mdb)
	}
}

func (mdb *plasmaSlice) cleanupWritersOnClose() {

	mdb.token.increment(mdb.numWriters)

	mdb.freeAllWriters()
	close(mdb.samplerStopCh)
}

//Destroy removes the database file from disk.
//Slice is not recoverable after this.
func (mdb *plasmaSlice) Destroy() {
	mdb.lock.Lock()
	defer mdb.lock.Unlock()

	if mdb.refCount > 0 {
		logging.Infof("plasmaSlice::Destroy Softdeleted Slice Id %v, IndexInstId %v, PartitionId %v "+
			"IndexDefnId %v", mdb.id, mdb.idxInstId, mdb.idxPartnId, mdb.idxDefnId)
		mdb.isSoftDeleted = true
	} else {
		tryDeleteplasmaSlice(mdb)
	}
}

//Id returns the Id for this Slice
func (mdb *plasmaSlice) Id() SliceId {
	return mdb.id
}

// FilePath returns the filepath for this Slice
func (mdb *plasmaSlice) Path() string {
	return mdb.path
}

//IsActive returns if the slice is active
func (mdb *plasmaSlice) IsActive() bool {
	return mdb.isActive
}

//SetActive sets the active state of this slice
func (mdb *plasmaSlice) SetActive(isActive bool) {
	mdb.isActive = isActive
}

//Status returns the status for this slice
func (mdb *plasmaSlice) Status() SliceStatus {
	return mdb.status
}

//SetStatus set new status for this slice
func (mdb *plasmaSlice) SetStatus(status SliceStatus) {
	mdb.status = status
}

//IndexInstId returns the Index InstanceId this
//slice is associated with
func (mdb *plasmaSlice) IndexInstId() common.IndexInstId {
	return mdb.idxInstId
}

//IndexDefnId returns the Index DefnId this slice
//is associated with
func (mdb *plasmaSlice) IndexDefnId() common.IndexDefnId {
	return mdb.idxDefnId
}

// IsDirty returns true if there has been any change in
// in the slice storage after last in-mem/persistent snapshot
func (mdb *plasmaSlice) IsDirty() bool {
	mdb.waitPersist()
	return mdb.isDirty
}

func (mdb *plasmaSlice) IsCompacting() bool {
	mdb.lock.Lock()
	defer mdb.lock.Unlock()
	return mdb.isCompacting
}

func (mdb *plasmaSlice) SetCompacting(compacting bool) {
	mdb.lock.Lock()
	defer mdb.lock.Unlock()
	mdb.isCompacting = compacting
}

func (mdb *plasmaSlice) IsSoftDeleted() bool {
	mdb.lock.Lock()
	defer mdb.lock.Unlock()
	return mdb.isSoftDeleted
}

func (mdb *plasmaSlice) IsSoftClosed() bool {
	mdb.lock.Lock()
	defer mdb.lock.Unlock()
	return mdb.isSoftClosed
}

func (mdb *plasmaSlice) Compact(abortTime time.Time, minFrag int) error {

	if mdb.IsCompacting() {
		return nil
	}

	var err error
	var wg sync.WaitGroup

	mdb.SetCompacting(true)
	defer mdb.SetCompacting(false)

	wg.Add(1)
	go func() {
		defer wg.Done()

		if mdb.mainstore.AutoLSSCleaning {
			return
		}

		shouldClean := func() bool {
			if mdb.IsSoftDeleted() || mdb.IsSoftClosed() {
				return false
			}
			return mdb.mainstore.TriggerLSSCleaner(minFrag, mdb.mainstore.LSSCleanerMinSize)
		}

		err = mdb.mainstore.CleanLSS(shouldClean)
	}()

	if !mdb.isPrimary && mdb.backstore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()

			if mdb.backstore.AutoLSSCleaning {
				return
			}

			shouldClean := func() bool {
				if mdb.IsSoftDeleted() || mdb.IsSoftClosed() {
					return false
				}
				return mdb.backstore.TriggerLSSCleaner(minFrag, mdb.backstore.LSSCleanerMinSize)
			}

			err = mdb.backstore.CleanLSS(shouldClean)
		}()
	}

	wg.Wait()

	return err
}

func (mdb *plasmaSlice) Statistics() (StorageStatistics, error) {
	var sts StorageStatistics

	var internalData []string

	var numRecsMem, numRecsDisk, cacheHits, cacheMiss int64
	pStats := mdb.mainstore.GetStats()

	numRecsMem += pStats.NumRecordAllocs - pStats.NumRecordFrees
	numRecsDisk += pStats.NumRecordSwapOut - pStats.NumRecordSwapIn
	cacheHits += pStats.CacheHits
	cacheMiss += pStats.CacheMisses
	sts.MemUsed = pStats.MemSz + pStats.MemSzIndex
	sts.InsertBytes = pStats.BytesWritten
	sts.GetBytes = pStats.LSSBlkReadBytes

	internalData = append(internalData, fmt.Sprintf("{\n\"MainStore\":\n%s", pStats))
	if !mdb.isPrimary {
		pStats := mdb.backstore.GetStats()
		numRecsMem += pStats.NumRecordAllocs - pStats.NumRecordFrees
		numRecsDisk += pStats.NumRecordSwapOut - pStats.NumRecordSwapIn
		cacheHits += pStats.CacheHits
		cacheMiss += pStats.CacheMisses
		sts.MemUsed += pStats.MemSz + pStats.MemSzIndex
		internalData = append(internalData, fmt.Sprintf(",\n\"BackStore\":\n%s", pStats))
		sts.InsertBytes += pStats.BytesWritten
		sts.GetBytes += pStats.LSSBlkReadBytes
	}

	internalData = append(internalData, "}\n")

	sts.InternalData = internalData
	if mdb.hasPersistence {
		_, sts.DataSize, sts.DiskSize = mdb.mainstore.GetLSSInfo()
		if !mdb.isPrimary {
			_, bsDataSz, bsDiskSz := mdb.backstore.GetLSSInfo()
			sts.DataSize += bsDataSz
			sts.DiskSize += bsDiskSz
		}
	}

	mdb.idxStats.residentPercent.Set(common.ComputePercent(numRecsMem, numRecsDisk))
	mdb.idxStats.cacheHitPercent.Set(common.ComputePercent(cacheHits, cacheMiss))
	mdb.idxStats.cacheHits.Set(cacheHits)
	mdb.idxStats.cacheMisses.Set(cacheMiss)
	mdb.idxStats.numRecsInMem.Set(numRecsMem)
	mdb.idxStats.numRecsOnDisk.Set(numRecsDisk)
	return sts, nil
}

func updatePlasmaConfig(cfg common.Config) {
	plasma.MTunerMaxFreeMemory = int64(cfg["plasma.memtuner.maxFreeMemory"].Int())
	plasma.MTunerMinFreeMemRatio = cfg["plasma.memtuner.minFreeRatio"].Float64()
	plasma.MTunerTrimDownRatio = cfg["plasma.memtuner.trimDownRatio"].Float64()
	plasma.MTunerIncrementRatio = cfg["plasma.memtuner.incrementRatio"].Float64()
	plasma.MTunerMinQuotaRatio = cfg["plasma.memtuner.minQuotaRatio"].Float64()
	plasma.MTunerOvershootRatio = cfg["plasma.memtuner.overshootRatio"].Float64()
	plasma.MTunerIncrCeilPercent = cfg["plasma.memtuner.incrCeilPercent"].Float64()
	plasma.MTunerMinQuota = int64(cfg["plasma.memtuner.minQuota"].Int())
}

func (mdb *plasmaSlice) UpdateConfig(cfg common.Config) {
	mdb.confLock.Lock()
	defer mdb.confLock.Unlock()

	mdb.sysconf = cfg

	updatePlasmaConfig(cfg)
	mdb.mainstore.AutoTuneLSSCleaning = cfg["plasma.AutoTuneLSSCleaner"].Bool()
	mdb.mainstore.MaxPageSize = cfg["plasma.MaxPageSize"].Int()

	mdb.mainstore.CheckpointInterval = time.Second * time.Duration(cfg["plasma.checkpointInterval"].Int())
	mdb.mainstore.MaxPageLSSSegments = mdb.sysconf["plasma.mainIndex.maxLSSPageSegments"].Int()
	mdb.mainstore.LSSCleanerThreshold = mdb.sysconf["plasma.mainIndex.LSSFragmentation"].Int()
	mdb.mainstore.LSSCleanerMaxThreshold = mdb.sysconf["plasma.mainIndex.maxLSSFragmentation"].Int()
	mdb.mainstore.DisableReadCaching = mdb.sysconf["plasma.disableReadCaching"].Bool()

	mdb.mainstore.PurgerInterval = time.Duration(mdb.sysconf["plasma.purger.interval"].Int()) * time.Second
	mdb.mainstore.PurgeThreshold = mdb.sysconf["plasma.purger.highThreshold"].Float64()
	mdb.mainstore.PurgeLowThreshold = mdb.sysconf["plasma.purger.lowThreshold"].Float64()
	mdb.mainstore.PurgeCompactRatio = mdb.sysconf["plasma.purger.compactRatio"].Float64()
	mdb.mainstore.MinEvictResidency = mdb.sysconf["plasma.minFgEvictResidentRatio"].Float64()
	mdb.mainstore.EnableLSSPageSMO = mdb.sysconf["plasma.enableLSSPageSMO"].Bool()

	if !mdb.isPrimary {
		mdb.backstore.AutoTuneLSSCleaning = cfg["plasma.AutoTuneLSSCleaner"].Bool()
		mdb.backstore.MaxPageSize = cfg["plasma.MaxPageSize"].Int()
		mdb.backstore.CheckpointInterval = mdb.mainstore.CheckpointInterval
		mdb.backstore.MaxPageLSSSegments = mdb.sysconf["plasma.backIndex.maxLSSPageSegments"].Int()
		mdb.backstore.LSSCleanerThreshold = mdb.sysconf["plasma.backIndex.LSSFragmentation"].Int()
		mdb.backstore.LSSCleanerMaxThreshold = mdb.sysconf["plasma.backIndex.maxLSSFragmentation"].Int()
		mdb.backstore.DisableReadCaching = mdb.sysconf["plasma.disableReadCaching"].Bool()

		mdb.backstore.PurgerInterval = time.Duration(mdb.sysconf["plasma.purger.interval"].Int()) * time.Second
		mdb.backstore.PurgeThreshold = mdb.sysconf["plasma.purger.highThreshold"].Float64()
		mdb.backstore.PurgeLowThreshold = mdb.sysconf["plasma.purger.lowThreshold"].Float64()
		mdb.backstore.PurgeCompactRatio = mdb.sysconf["plasma.purger.compactRatio"].Float64()
		mdb.backstore.MinEvictResidency = mdb.sysconf["plasma.minFgEvictResidentRatio"].Float64()
		mdb.backstore.EnableLSSPageSMO = mdb.sysconf["plasma.enableLSSPageSMO"].Bool()
	}
	mdb.maxRollbacks = cfg["settings.plasma.recovery.max_rollbacks"].Int()
}

func (mdb *plasmaSlice) String() string {

	str := fmt.Sprintf("SliceId: %v ", mdb.id)
	str += fmt.Sprintf("File: %v ", mdb.path)
	str += fmt.Sprintf("Index: %v ", mdb.idxInstId)
	str += fmt.Sprintf("Partition: %v ", mdb.idxPartnId)

	return str

}

func tryDeleteplasmaSlice(mdb *plasmaSlice) {
	//cleanup the disk directory
	if err := os.RemoveAll(mdb.path); err != nil {
		logging.Errorf("plasmaSlice::Destroy Error Cleaning Up Slice Id %v, "+
			"IndexInstId %v, PartitionId %v, IndexDefnId %v. Error %v", mdb.id, mdb.idxInstId, mdb.idxPartnId, mdb.idxDefnId, err)
	}
}

func tryCloseplasmaSlice(mdb *plasmaSlice) {
	mdb.waitForPersistorThread()
	mdb.mainstore.Close()

	if !mdb.isPrimary {
		mdb.backstore.Close()
	}
}

func (mdb *plasmaSlice) getCmdsCount() int {
	qc := atomic.LoadInt64(&mdb.qCount)
	return int(qc)
}

func (mdb *plasmaSlice) logWriterStat() {
	count := atomic.AddUint64(&mdb.flushedCount, 1)
	if (count%10000 == 0) || count == 1 {
		logging.Debugf("logWriterStat:: %v:%v "+
			"FlushedCount %v QueuedCount %v", mdb.idxInstId, mdb.idxPartnId,
			count, mdb.getCmdsCount())
	}

}

func (info *plasmaSnapshotInfo) Timestamp() *common.TsVbuuid {
	return info.Ts
}

func (info *plasmaSnapshotInfo) IsCommitted() bool {
	return info.Committed
}

func (info *plasmaSnapshotInfo) String() string {
	return fmt.Sprintf("SnapshotInfo: count:%v committed:%v", info.Count, info.Committed)
}

func (s *plasmaSnapshot) Create() error {
	return nil
}

func (s *plasmaSnapshot) Open() error {
	atomic.AddInt32(&s.refCount, int32(1))

	return nil
}

func (s *plasmaSnapshot) IsOpen() bool {

	count := atomic.LoadInt32(&s.refCount)
	return count > 0
}

func (s *plasmaSnapshot) Id() SliceId {
	return s.slice.Id()
}

func (s *plasmaSnapshot) IndexInstId() common.IndexInstId {
	return s.idxInstId
}

func (s *plasmaSnapshot) IndexDefnId() common.IndexDefnId {
	return s.idxDefnId
}

func (s *plasmaSnapshot) Timestamp() *common.TsVbuuid {
	return s.ts
}

func (s *plasmaSnapshot) Close() error {

	count := atomic.AddInt32(&s.refCount, int32(-1))

	if count < 0 {
		logging.Errorf("plasmaSnapshot::Close Close operation requested " +
			"on already closed snapshot")
		return errors.New("Snapshot Already Closed")

	} else if count == 0 {
		s.Destroy()
	}

	return nil
}

func (mdb *plasmaSlice) waitForPersistorThread() {
	for atomic.LoadInt32(&mdb.isPersistorActive) == 1 {
		time.Sleep(time.Second)
	}
}

func (s *plasmaSnapshot) Destroy() {
	s.MainSnap.Close()
	if s.BackSnap != nil {
		s.BackSnap.Close()
	}

	defer s.slice.DecrRef()
}

func (s *plasmaSnapshot) String() string {

	str := fmt.Sprintf("Index: %v ", s.idxInstId)
	str += fmt.Sprintf("PartitionId: %v ", s.idxPartnId)
	str += fmt.Sprintf("SliceId: %v ", s.slice.Id())
	str += fmt.Sprintf("TS: %v ", s.ts)
	return str
}

func (s *plasmaSnapshot) Info() SnapshotInfo {
	return s.info
}

// ==============================
// Snapshot reader implementation
// ==============================

// Approximate items count
func (s *plasmaSnapshot) StatCountTotal() (uint64, error) {
	c := s.slice.GetCommittedCount()
	return c, nil
}

func (s *plasmaSnapshot) CountTotal(ctx IndexReaderContext, stopch StopChannel) (uint64, error) {
	return uint64(s.MainSnap.Count()), nil
}

func (s *plasmaSnapshot) CountRange(ctx IndexReaderContext, low, high IndexKey, inclusion Inclusion,
	stopch StopChannel) (uint64, error) {

	var count uint64
	callb := func([]byte) error {
		select {
		case <-stopch:
			return common.ErrClientCancel
		default:
			count++
		}

		return nil
	}

	err := s.Range(ctx, low, high, inclusion, callb)
	return count, err
}

func (s *plasmaSnapshot) MultiScanCount(ctx IndexReaderContext, low, high IndexKey, inclusion Inclusion,
	scan Scan, distinct bool,
	stopch StopChannel) (uint64, error) {

	var err error
	var scancount uint64
	count := 1
	checkDistinct := distinct && !s.isPrimary()
	isIndexComposite := len(s.slice.idxDefn.SecExprs) > 1

	buf := secKeyBufPool.Get()
	defer secKeyBufPool.Put(buf)

	previousRow := ctx.GetCursorKey()

	revbuf := secKeyBufPool.Get()
	defer secKeyBufPool.Put(revbuf)

	callb := func(entry []byte) error {
		select {
		case <-stopch:
			return common.ErrClientCancel
		default:
			skipRow := false
			var ck [][]byte

			//get the key in original format
			// TODO: ONLY if scan.ScanType == FilterRangeReq || (checkDistinct && isIndexComposite) {
			if s.slice.idxDefn.Desc != nil {
				revbuf := (*revbuf)[:0]
				//copy is required, otherwise storage may get updated
				revbuf = append(revbuf, entry...)
				jsonEncoder.ReverseCollate(revbuf, s.slice.idxDefn.Desc)
				entry = revbuf
			}
			if scan.ScanType == FilterRangeReq {
				if len(entry) > cap(*buf) {
					*buf = make([]byte, 0, len(entry)+RESIZE_PAD)
				}

				skipRow, ck, err = filterScanRow(entry, scan, (*buf)[:0])
				if err != nil {
					return err
				}
			}
			if skipRow {
				return nil
			}

			if checkDistinct {
				if isIndexComposite {
					// For Count Distinct, only leading key needs to be considered for
					// distinct comparison as N1QL syntax supports distinct on only single key
					entry, err = projectLeadingKey(ck, entry, buf)
				}
				if len(*previousRow) != 0 && distinctCompare(entry, *previousRow) {
					return nil // Ignore the entry as it is same as previous entry
				}
			}

			if !s.isPrimary() {
				e := secondaryIndexEntry(entry)
				count = e.Count()
			}

			if checkDistinct {
				scancount++
				*previousRow = append((*previousRow)[:0], entry...)
			} else {
				scancount += uint64(count)
			}
		}
		return nil
	}
	e := s.Range(ctx, low, high, inclusion, callb)
	return scancount, e
}

func (s *plasmaSnapshot) CountLookup(ctx IndexReaderContext, keys []IndexKey, stopch StopChannel) (uint64, error) {
	var err error
	var count uint64

	callb := func([]byte) error {
		select {
		case <-stopch:
			return common.ErrClientCancel
		default:
			count++
		}

		return nil
	}

	for _, k := range keys {
		if err = s.Lookup(ctx, k, callb); err != nil {
			break
		}
	}

	return count, err
}

func (s *plasmaSnapshot) Exists(ctx IndexReaderContext, key IndexKey, stopch StopChannel) (bool, error) {
	var count uint64
	callb := func([]byte) error {
		select {
		case <-stopch:
			return common.ErrClientCancel
		default:
			count++
		}

		return nil
	}

	err := s.Lookup(ctx, key, callb)
	return count != 0, err
}

func (s *plasmaSnapshot) Lookup(ctx IndexReaderContext, key IndexKey, callb EntryCallback) error {
	return s.Iterate(ctx, key, key, Both, compareExact, callb)
}

func (s *plasmaSnapshot) Range(ctx IndexReaderContext, low, high IndexKey, inclusion Inclusion,
	callb EntryCallback) error {

	var cmpFn CmpEntry
	if s.isPrimary() {
		cmpFn = compareExact
	} else {
		cmpFn = comparePrefix
	}

	return s.Iterate(ctx, low, high, inclusion, cmpFn, callb)
}

func (s *plasmaSnapshot) All(ctx IndexReaderContext, callb EntryCallback) error {
	return s.Range(ctx, MinIndexKey, MaxIndexKey, Both, callb)
}

func (s *plasmaSnapshot) Iterate(ctx IndexReaderContext, low, high IndexKey, inclusion Inclusion,
	cmpFn CmpEntry, callback EntryCallback) error {
	var entry IndexEntry
	var err error
	t0 := time.Now()

	reader := ctx.(*plasmaReaderCtx)

	it, err := reader.r.NewSnapshotIterator(s.MainSnap)

	// Snapshot became invalid due to rollback
	if err == plasma.ErrInvalidSnapshot {
		return ErrIndexRollback
	}

	defer it.Close()

	endKey := high.Bytes()
	if len(endKey) > 0 {
		if inclusion == High || inclusion == Both {
			endKey = common.GenNextBiggerKey(endKey, s.isPrimary())
		}

		it.SetEndKey(endKey)
	}

	if len(low.Bytes()) == 0 {
		it.SeekFirst()
	} else {
		it.Seek(low.Bytes())

		// Discard equal keys if low inclusion is requested
		if inclusion == Neither || inclusion == High {
			err = s.iterEqualKeys(low, it, cmpFn, nil)
			if err != nil {
				return err
			}
		}
	}
	s.slice.idxStats.Timings.stNewIterator.Put(time.Since(t0))

loop:
	for it.Valid() {
		itm := it.Key()
		s.newIndexEntry(itm, &entry)

		// Iterator has reached past the high key, no need to scan further
		if cmpFn(high, entry) <= 0 {
			break loop
		}

		err = callback(entry.Bytes())
		if err != nil {
			return err
		}

		it.Next()
	}

	// Include equal keys if high inclusion is requested
	if inclusion == Both || inclusion == High {
		err = s.iterEqualKeys(high, it, cmpFn, callback)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *plasmaSnapshot) isPrimary() bool {
	return s.slice.isPrimary
}

func (s *plasmaSnapshot) newIndexEntry(b []byte, entry *IndexEntry) {
	var err error

	if s.slice.isPrimary {
		*entry, err = BytesToPrimaryIndexEntry(b)
	} else {
		*entry, err = BytesToSecondaryIndexEntry(b)
	}
	common.CrashOnError(err)
}

func (s *plasmaSnapshot) iterEqualKeys(k IndexKey, it *plasma.MVCCIterator,
	cmpFn CmpEntry, callback func([]byte) error) error {
	var err error

	var entry IndexEntry
	for ; it.Valid(); it.Next() {
		itm := it.Key()
		s.newIndexEntry(itm, &entry)
		if cmpFn(k, entry) == 0 {
			if callback != nil {
				err = callback(itm)
				if err != nil {
					return err
				}
			}
		} else {
			break
		}
	}

	return err
}

// TODO: Cleanup the leaky hack to reuse the buffer
// Extract only secondary key
func entry2BackEntry(entry secondaryIndexEntry) []byte {
	buf := entry.Bytes()
	kl := entry.lenKey()
	if entry.isCountEncoded() {
		// Store count
		dl := entry.lenDocId()
		copy(buf[kl:kl+2], buf[kl+dl:kl+dl+2])
		return buf[:kl+2]
	} else {
		// Set count to 0
		buf[kl] = 0
		buf[kl+1] = 0
	}

	return buf[:kl+2]
}

// Reformat secondary key to entry
func backEntry2entry(docid []byte, bentry []byte, buf []byte) []byte {
	l := len(bentry)
	count := int(binary.LittleEndian.Uint16(bentry[l-2 : l]))
	entry, _ := NewSecondaryIndexEntry2(bentry[:l-2], docid, false, count, nil, buf[:0], false, nil)
	return entry.Bytes()
}

func hasEqualBackEntry(key []byte, bentry []byte) bool {
	if key == nil || isJSONEncoded(key) {
		return false
	}

	// Ignore 2 byte count for comparison
	return bytes.Equal(key, bentry[:len(bentry)-2])
}

////////////////////////////////////////////////////////////
// Writer Auto-Tuning
////////////////////////////////////////////////////////////

//
// Default number of num writers
//
func (slice *plasmaSlice) numWritersPerPartition() int {
	return int(math.Ceil(float64(slice.maxNumWriters) / float64(slice.numPartitions)))
}

//
// Get command handler queue size
//
func (slice *plasmaSlice) defaultCmdQueueSize() uint64 {

	sliceBufSize := slice.sysconf["settings.sliceBufSize"].Uint64()
	numWriters := slice.numWritersPerPartition()

	if sliceBufSize < uint64(numWriters) {
		sliceBufSize = uint64(numWriters)
	}

	return sliceBufSize / uint64(numWriters)
}

//
// Allocate array for writers
//
func (slice *plasmaSlice) setupWriters() {

	// initialize buffer
	slice.encodeBuf = make([][]byte, 0, slice.maxNumWriters)
	slice.arrayBuf1 = make([][]byte, 0, slice.maxNumWriters)
	slice.arrayBuf2 = make([][]byte, 0, slice.maxNumWriters)

	// initialize comand handler
	slice.cmdCh = make([]chan indexMutation, 0, slice.maxNumWriters)
	slice.workerDone = make([]chan bool, 0, slice.maxNumWriters)
	slice.stopCh = make([]DoneChannel, 0, slice.maxNumWriters)

	// initialize writers
	slice.main = make([]*plasma.Writer, 0, slice.maxNumWriters)
	slice.back = make([]*plasma.Writer, 0, slice.maxNumWriters)

	// initialize tokens
	slice.token = registerFreeWriters(slice.idxInstId, slice.maxNumWriters)

	// start writers
	numWriter := slice.numWritersPerPartition()
	slice.token.decrement(numWriter, true)
	slice.startWriters(numWriter)

	// start stats sampler
	go slice.runSampler()
}

//
// Initialize any field related to numWriters
//
func (slice *plasmaSlice) initWriters(numWriters int) {

	curNumWriters := len(slice.cmdCh)

	// initialize buffer
	slice.encodeBuf = slice.encodeBuf[:numWriters]
	slice.arrayBuf1 = slice.arrayBuf1[:numWriters]
	slice.arrayBuf2 = slice.arrayBuf2[:numWriters]
	for i := curNumWriters; i < numWriters; i++ {
		slice.encodeBuf[i] = make([]byte, 0, maxIndexEntrySize)
		slice.arrayBuf1[i] = make([]byte, 0, maxArrayIndexEntrySize)
		slice.arrayBuf2[i] = make([]byte, 0, maxArrayIndexEntrySize)
	}

	// initialize command handler
	queueSize := slice.defaultCmdQueueSize()
	slice.cmdCh = slice.cmdCh[:numWriters]
	slice.workerDone = slice.workerDone[:numWriters]
	slice.stopCh = slice.stopCh[:numWriters]
	for i := curNumWriters; i < numWriters; i++ {
		slice.cmdCh[i] = make(chan indexMutation, queueSize)
		slice.workerDone[i] = make(chan bool)
		slice.stopCh[i] = make(DoneChannel)

		go slice.handleCommandsWorker(i)
	}

	// initialize mainsotre workers
	slice.main = slice.main[:numWriters]
	for i := curNumWriters; i < numWriters; i++ {
		slice.main[i] = slice.mainstore.NewWriter()
	}

	// initialize backstore writers
	if !slice.isPrimary {
		slice.back = slice.back[:numWriters]
		for i := curNumWriters; i < numWriters; i++ {
			slice.back[i] = slice.backstore.NewWriter()
		}
	}
}

//
// Start the writers by passing in the desired number of writers
//
func (slice *plasmaSlice) startWriters(numWriters int) {

	// If slice already have more writers that the desired number, return.
	if slice.numWriters >= numWriters {
		return
	}

	// If desired number is more than length of the slice, then need to resize.
	if numWriters > len(slice.cmdCh) {
		slice.stopWriters(0)
		slice.initWriters(numWriters)
	}

	// update the number of slice writers
	slice.numWriters = numWriters
}

//
// Stop the writers by passing in the desired number of writers
//
func (slice *plasmaSlice) stopWriters(numWriters int) {

	// If slice already have fewer writers that the desired number, return.
	if numWriters >= slice.numWriters {
		return
	}

	// free writer memory
	for i := numWriters; i < slice.numWriters; i++ {
		slice.main[i].ResetBuffers()
		if !slice.isPrimary {
			slice.back[i].ResetBuffers()
		}
	}

	// update the number of slice writers
	slice.numWriters = numWriters
}

//
// Free all writers
//
func (slice *plasmaSlice) freeAllWriters() {
	// Stop all command workers
	for _, stopCh := range slice.stopCh {
		stopCh <- true
		<-stopCh
	}

	slice.stopWriters(0)

	slice.encodeBuf = slice.encodeBuf[:0]
	slice.arrayBuf1 = slice.arrayBuf1[:0]
	slice.arrayBuf2 = slice.arrayBuf2[:0]

	slice.cmdCh = slice.cmdCh[:0]
	slice.workerDone = slice.workerDone[:0]
	slice.stopCh = slice.stopCh[:0]

	slice.main = slice.main[:0]
	if !slice.isPrimary {
		slice.back = slice.back[:0]
	}
}

//
// Logging
//
func (slice *plasmaSlice) logSample(numWriters int) {

	logging.Infof("plasmaSlice %v:%v mutation rate %.2f drain rate %.2f saturateCount %v minimum drain rate %.2f",
		slice.idxInstId, slice.idxPartnId,
		slice.adjustedMeanMutationRate(),
		slice.adjustedMeanDrainRate()*float64(numWriters),
		slice.saturateCount,
		slice.minimumDrainRate)
}

//
// Expand the number of writer
//
func (slice *plasmaSlice) expandWriters(needed int) {

	// increment writer one at a 1 to avoid saturation.    This means that
	// it will be less responsive for sporadic traffic.  It will take
	// longer for stale=false query to catch up when there is a spike in
	// mutation rate.

	//increment := int(needed - slice.numWriters)
	increment := 1

	mean := slice.adjustedMeanDrainRate() * float64(slice.numWriters)
	if increment > 0 && mean > 0 {
		// Is there any free writer available?
		if increment = slice.token.decrement(increment, false); increment > 0 {
			lastNumWriters := slice.numWriters

			// start writer
			slice.startWriters(slice.numWriters + increment)

			slice.minimumDrainRate = slice.computeMinimumDrainRate(lastNumWriters)
			slice.numExpand++

			logging.Verbosef("plasmaSlice %v:%v expand writers from %v to %v (standby writer %v) token %v",
				slice.idxInstId, slice.idxPartnId, lastNumWriters, slice.numWriters,
				len(slice.cmdCh)-slice.numWriters, slice.token.num())

			if logging.IsEnabled(logging.Verbose) {
				slice.logSample(lastNumWriters)
			}
		}
	}
}

//
// Reduce the number of writer
//
func (slice *plasmaSlice) reduceWriters(needed int) {

	//decrement := int(math.Ceil(float64(slice.numWriters-needed) / 2))
	decrement := 1

	if decrement > 0 {
		lastNumWriters := slice.numWriters

		// stop writer
		slice.stopWriters(slice.numWriters - decrement)

		// add token after the writer is freed
		slice.token.increment(decrement)

		slice.minimumDrainRate = slice.computeMinimumDrainRate(lastNumWriters)
		slice.numReduce++

		logging.Verbosef("plasmaSlice %v:%v reduce writers from %v to %v (standby writer %v) token %v",
			slice.idxInstId, slice.idxPartnId, lastNumWriters, slice.numWriters,
			len(slice.cmdCh)-slice.numWriters, slice.token.num())

		if logging.IsEnabled(logging.Verbose) {
			slice.logSample(lastNumWriters)
		}
	}
}

//
// Calculate minimum drain rate
// Minimum drain rate is calculated everytime when expanding or reducing writers, so it keeps
// adjusting to the trailing 1 second mean drain rate. If drain rate is trending down,
// then minimum drain rate will also trending down.
//
func (slice *plasmaSlice) computeMinimumDrainRate(lastNumWriters int) float64 {

	// compute expected drain rate based on mean drain rate adjusted based on memory usage
	mean := slice.adjustedMeanDrainRate() * float64(lastNumWriters)
	newMean := mean * float64(slice.numWriters) / float64(lastNumWriters)

	if slice.numWriters > lastNumWriters {
		return mean + ((newMean - mean) * slice.scalingFactor)
	}

	return newMean
}

//
// Does drain rate meet the minimum level?
//
func (slice *plasmaSlice) meetMinimumDrainRate() {

	// If the slice does not meet the minimum drain rate requirement after expanding/reducing writers, increment
	// saturation count.  Saturation count is token to keep track of how many misses on minimum drain rate.
	// If drain rate is not saturated or trending down, normal flucturation in drain rate should not keep
	// saturation count reaching threshold.
	//
	// The minimum drain rate is computed to be an easy-to-reach target in order to reduce chances of false
	// positive on drain rate saturation.
	//
	// If drain rate is saturated or trending down, there will be more misses than hits.  The saturation count should increase,
	// since the minimum drain rate is trailing the actual drain rate.
	//
	if slice.adjustedMeanDrainRateWithInterval(slice.adjustInterval)*float64(slice.numWriters) < slice.minimumDrainRate {
		if slice.saturateCount < slice.threshold {
			slice.saturateCount++
		}
	} else {
		if slice.saturateCount > 0 {
			slice.saturateCount--
		}
	}
}

//
// Adjust number of writers needed
//
func (slice *plasmaSlice) adjustNumWritersNeeded(needed int) int {

	// Find a victim to release token if running out of token
	if slice.token.num() < 0 {
		if float64(slice.numWriters)/float64(slice.maxNumWriters) > rand.Float64() {
			return slice.numWriters - 1
		}
	}

	// do not allow expansion when reaching minimum memory
	if slice.minimumMemory() && needed > slice.numWriters {
		return slice.numWriters
	}

	// limit writer when memory is 95% full
	if slice.memoryFull() &&
		needed > slice.numWriters &&
		needed > slice.numWritersPerPartition() {

		if slice.numWriters > slice.numWritersPerPartition() {
			return slice.numWriters
		}

		return slice.numWritersPerPartition()
	}

	// There are different situations where drain rate goes down and cannot meet minimum requiremnts:
	// 1) IO saturation
	// 2) new plasma instance is added to the node
	// 3) log cleaner running
	// 4) DGM ratio goes down
	//
	// If it gets 10 misses, then it could mean the drain rate has saturated or trending down over 1s interval.
	// If so, redcue the number of writers by 1, and re-calibrate by recomputing the minimum drain rate again.
	// In the next interval, if the 100ms drain rate is able to meet the minimum requirement, it will allow
	// number of writers to expand. Otherwise, it will keep reducing the number of writers until it can meet
	// the minimum drain rate.
	//
	/*
		if slice.saturateCount >= slice.threshold {
			return slice.numWriters - 1
		}
	*/

	return needed
}

//
// Adjust the number of writer
//
func (slice *plasmaSlice) adjustWriters() {

	slice.writerLock.Lock()
	defer slice.writerLock.Unlock()

	// Is it the time to adjust the number of writers?
	if slice.shouldAdjustWriter() {
		slice.meetMinimumDrainRate()

		needed := slice.numWritersNeeded()
		needed = slice.adjustNumWritersNeeded(needed)

		if slice.canExpandWriters(needed) {
			slice.expandWriters(needed)
		} else if slice.canReduceWriters(needed) {
			slice.reduceWriters(needed)
		}
	}
}

//
// Expand the writer when
// 1) enableWriterTuning is enabled
// 2) numWriters is fewer than the maxNumWriters
// 3) numWriters needed is greater than numWriters
// 4) drain rate has increased since the last expansion
//
func (slice *plasmaSlice) canExpandWriters(needed int) bool {

	return slice.enableWriterTuning &&
		slice.numWriters < slice.maxNumWriters &&
		needed > slice.numWriters
}

//
// Reduce the writer when
// 1) enableWriterTuning is enabled
// 2) numWriters is greater than 1
// 3) numWriters needed is fewer than numWriters
//
func (slice *plasmaSlice) canReduceWriters(needed int) bool {

	return slice.enableWriterTuning &&
		slice.numWriters > 1 &&
		needed < slice.numWriters
}

//
// Update the sample based on the stats collected in last flush
// Drain rate and mutation rate is measured based on the
// number of incoming and written keys.   It does not include
// the size of the key.
//
func (slice *plasmaSlice) updateSample(elapsed int64, needLog bool) {

	slice.writerLock.Lock()
	defer slice.writerLock.Unlock()

	drainTime := float64(atomic.LoadInt64(&slice.drainTime))
	mutations := float64(atomic.LoadInt64(&slice.numItems))

	// Update the drain rate.
	drainRate := float64(0)
	if drainTime > 0 {
		// drain rate = num of items written per writer per second
		drainRate = mutations / drainTime * float64(slice.snapInterval)
	}
	drainRatePerWriter := drainRate / float64(slice.numWriters)
	slice.drainRate.Update(drainRatePerWriter)

	// Update mutation rate.
	mutationRate := mutations / float64(elapsed) * float64(slice.snapInterval)
	slice.mutationRate.Update(mutationRate)

	// reset stats
	atomic.StoreInt64(&slice.drainTime, 0)
	atomic.StoreInt64(&slice.numItems, 0)

	// periodic logging
	if needLog {
		logging.Infof("plasmaSlice %v:%v numWriter %v standby writer %v token %v numExpand %v numReduce %v",
			slice.idxInstId, slice.idxPartnId, slice.numWriters, len(slice.cmdCh)-slice.numWriters, slice.token.num(),
			slice.numExpand, slice.numReduce)

		slice.logSample(slice.numWriters)

		slice.numExpand = 0
		slice.numReduce = 0
	}
}

//
// Check if it is time to adjust the writer
//
func (slice *plasmaSlice) shouldAdjustWriter() bool {

	if !slice.enableWriterTuning {
		return false
	}

	now := time.Now().UnixNano()
	if now-slice.lastCheckTime > int64(slice.adjustInterval) {
		slice.lastCheckTime = now
		return true
	}

	return false
}

//
// Mutation rate is always calculated using adjust interval (100ms), adjusted based on memory utilization.
// Short interval for mutation rate alllows more responsiveness. Drain rate is calculated at 1s interval to
// reduce variation.   Therefore, fluctation in mutation rate is more likely to cause writers to expand/reduce
// than fluctation in drain rate. The implementation attempts to make allocate/de-allocate writers efficiently
// to faciliate constant expansion/reduction of writers.
//
func (slice *plasmaSlice) numWritersNeeded() int {

	mutationRate := slice.adjustedMeanMutationRate()
	drainRate := slice.adjustedMeanDrainRate()

	// If drain rate is 0, there is no expansion.
	if drainRate > 0 {
		needed := int(math.Ceil(mutationRate / drainRate))

		if needed == 0 {
			needed = 1
		}

		if needed > slice.maxNumWriters {
			needed = slice.maxNumWriters
		}

		return needed
	}

	// return 1 if there is no mutation
	if mutationRate <= 0 {
		return 1
	}

	// If drain rate is 0 but mutation rate is not 0, then return current numWriters
	return slice.numWriters
}

//
// Run sampler every second

func (slice *plasmaSlice) runSampler() {

	if !slice.enableWriterTuning {
		return
	}

	lastTime := time.Now()
	lastLogTime := lastTime

	ticker := time.NewTicker(time.Duration(slice.samplingInterval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			needLog := time.Now().Sub(lastLogTime).Nanoseconds() > int64(time.Minute)
			slice.updateSample(time.Now().Sub(lastTime).Nanoseconds(), needLog)
			lastTime = time.Now()
			if needLog {
				lastLogTime = lastTime
			}
		case <-slice.samplerStopCh:
			return
		}
	}
}

type windowFunc func(sample *common.Sample, count int) float64

//
// Get mean drain rate adjusted based on memory uasge
//
func (slice *plasmaSlice) adjustedMeanDrainRate() float64 {

	return slice.adjustedMeanDrainRateWithInterval(uint64(time.Second))
}

func (slice *plasmaSlice) adjustedMeanDrainRateWithInterval(interval uint64) float64 {

	window := func(sample *common.Sample, count int) float64 { return sample.WindowMean(count) }
	return slice.computeAdjustedAggregate(window, slice.drainRate, interval)
}

//
// Get std dev drain rate adjusted based on memory uasge
//
func (slice *plasmaSlice) adjustedStdDevDrainRate() float64 {

	window := func(sample *common.Sample, count int) float64 { return sample.WindowStdDev(count) }
	return slice.computeAdjustedAggregate(window, slice.drainRate, uint64(time.Second))
}

//
// Get mean mutation rate adjusted based on memory uasge
//
func (slice *plasmaSlice) adjustedMeanMutationRate() float64 {

	window := func(sample *common.Sample, count int) float64 { return sample.WindowMean(count) }
	return slice.computeAdjustedAggregate(window, slice.mutationRate, slice.adjustInterval)
}

//
// Get std dev mutation rate adjusted based on memory uasge
//
func (slice *plasmaSlice) adjustedStdDevMutationRate() float64 {

	window := func(sample *common.Sample, count int) float64 { return sample.WindowStdDev(count) }
	return slice.computeAdjustedAggregate(window, slice.mutationRate, slice.adjustInterval)
}

func (slice *plasmaSlice) computeAdjustedAggregate(window windowFunc, sample *common.Sample, interval uint64) float64 {

	count := int(interval / slice.samplingInterval)

	if float64(slice.memoryAvail()) < float64(slice.memoryLimit())*0.20 && slice.memoryAvail() > 0 {
		count = count * int(slice.memoryLimit()/slice.memoryAvail())
		if count > int(slice.samplingWindow/slice.samplingInterval) {
			count = int(slice.samplingWindow / slice.samplingInterval)
		}
	}

	return window(sample, count)
}

//
// get memory limit
//
func (slice *plasmaSlice) memoryLimit() float64 {

	//return float64(slice.indexerStats.memoryQuota.Value())
	return float64(getMemTotal())
}

//
// get available memory left
//
func (slice *plasmaSlice) memoryAvail() float64 {

	//return float64(slice.indexerStats.memoryQuota.Value()) - float64(slice.indexerStats.memoryUsed.Value())
	return float64(getMemFree())
}

//
// get memory used
//
func (slice *plasmaSlice) memoryUsed() float64 {

	//return float64(slice.indexerStats.memoryUsed.Value())
	return slice.memoryLimit() - slice.memoryAvail()
}

//
// memory full
//
func (slice *plasmaSlice) memoryFull() bool {

	return (float64(slice.memoryAvail()) < float64(slice.memoryLimit())*0.05)
}

//
// minimum memory  (10M)
//
func (slice *plasmaSlice) minimumMemory() bool {

	return (float64(slice.memoryAvail()) <= float64(20*1024*1024))
}

////////////////////////////////////////////////////////////
// Writer Tokens
////////////////////////////////////////////////////////////

var freeWriters tokens

func init() {
	freeWriters.tokens = make(map[common.IndexInstId]*token)
}

type token struct {
	value int64
}

func (t *token) num() int64 {
	return atomic.LoadInt64(&t.value)
}

func (t *token) increment(increment int) {

	atomic.AddInt64(&t.value, int64(increment))
}

func (t *token) decrement(decrement int, force bool) int {

	for {
		if count := atomic.LoadInt64(&t.value); count > 0 || force {
			d := int64(decrement)

			if !force {
				if int64(decrement) > count {
					d = count
				}
			}

			if atomic.CompareAndSwapInt64(&t.value, count, count-d) {
				return int(d)
			}
		} else {
			break
		}
	}

	return 0
}

type tokens struct {
	mutex  sync.RWMutex
	tokens map[common.IndexInstId]*token
}

func registerFreeWriters(instId common.IndexInstId, count int) *token {

	freeWriters.mutex.Lock()
	defer freeWriters.mutex.Unlock()

	if _, ok := freeWriters.tokens[instId]; !ok {
		freeWriters.tokens[instId] = &token{value: int64(count)}
	}
	return freeWriters.tokens[instId]
}

func deleteFreeWriters(instId common.IndexInstId) {
	freeWriters.mutex.Lock()
	defer freeWriters.mutex.Unlock()
	delete(freeWriters.tokens, instId)
}
