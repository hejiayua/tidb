// Copyright 2016 PingCAP, Inc.
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

package executor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/expression"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tidb/util/stringutil"
)

var (
	_ Executor = &HashJoinExec{}
	_ Executor = &NestedLoopApplyExec{}
)

// HashJoinExec implements the hash join algorithm.
type HashJoinExec struct {
	baseExecutor

	probeSideExec     Executor
	buildSideExec     Executor
	buildSideEstCount float64
	probeSideFilter   expression.CNFExprs
	probeKeys         []*expression.Column
	buildKeys         []*expression.Column

	// concurrency is the number of partition, build and join workers.
	concurrency   uint
	rowContainer  *hashRowContainer
	buildFinished chan error
	// joinWorkerWaitGroup is for sync multiple join workers.
	joinWorkerWaitGroup sync.WaitGroup
	finished            atomic.Value
	// closeCh add a lock for closing executor.
	closeCh      chan struct{}
	joinType     plannercore.JoinType
	requiredRows int64

	// We build individual joiner for each join worker when use chunk-based
	// execution, to avoid the concurrency of joiner.chk and joiner.selected.
	joiners []joiner

	probeChkResourceCh chan *probeChkResource
	probeResultChs     []chan *chunk.Chunk
	joinChkResourceCh  []chan *chunk.Chunk
	joinResultCh       chan *hashjoinWorkerResult

	memTracker  *memory.Tracker // track memory usage.
	prepared    bool
	isOuterJoin bool
}

// probeChkResource stores the result of the join probe side fetch worker,
// `dest` is for Chunk reuse: after join workers process the probe side chunk which is read from `dest`,
// they'll store the used chunk as `chk`, and then the probe side fetch worker will put new data into `chk` and write `chk` into dest.
type probeChkResource struct {
	chk  *chunk.Chunk
	dest chan<- *chunk.Chunk
}

// hashjoinWorkerResult stores the result of join workers,
// `src` is for Chunk reuse: the main goroutine will get the join result chunk `chk`,
// and push `chk` into `src` after processing, join worker goroutines get the empty chunk from `src`
// and push new data into this chunk.
type hashjoinWorkerResult struct {
	chk *chunk.Chunk
	err error
	src chan<- *chunk.Chunk
}

// Close implements the Executor Close interface.
func (e *HashJoinExec) Close() error {
	close(e.closeCh)
	e.finished.Store(true)
	if e.prepared {
		if e.buildFinished != nil {
			for range e.buildFinished {
			}
		}
		if e.joinResultCh != nil {
			for range e.joinResultCh {
			}
		}
		if e.probeChkResourceCh != nil {
			close(e.probeChkResourceCh)
			for range e.probeChkResourceCh {
			}
		}
		for i := range e.probeResultChs {
			for range e.probeResultChs[i] {
			}
		}
		for i := range e.joinChkResourceCh {
			close(e.joinChkResourceCh[i])
			for range e.joinChkResourceCh[i] {
			}
		}
		e.probeChkResourceCh = nil
		e.joinChkResourceCh = nil
		terror.Call(e.rowContainer.Close)
	}

	err := e.baseExecutor.Close()
	return err
}

// Open implements the Executor Open interface.
func (e *HashJoinExec) Open(ctx context.Context) error {
	if err := e.baseExecutor.Open(ctx); err != nil {
		return err
	}

	e.prepared = false
	e.memTracker = memory.NewTracker(e.id, -1)
	e.memTracker.AttachTo(e.ctx.GetSessionVars().StmtCtx.MemTracker)

	e.closeCh = make(chan struct{})
	e.finished.Store(false)
	e.joinWorkerWaitGroup = sync.WaitGroup{}
	return nil
}

// fetchProbeSideChunks get chunks from fetches chunks from the big table in a background goroutine
// and sends the chunks to multiple channels which will be read by multiple join workers.
func (e *HashJoinExec) fetchProbeSideChunks(ctx context.Context) {
	hasWaitedForBuild := false
	for {
		if e.finished.Load().(bool) {
			return
		}

		var probeSideResource *probeChkResource
		var ok bool
		select {
		case <-e.closeCh:
			return
		case probeSideResource, ok = <-e.probeChkResourceCh:
			if !ok {
				return
			}
		}
		probeSideResult := probeSideResource.chk
		if e.isOuterJoin {
			required := int(atomic.LoadInt64(&e.requiredRows))
			probeSideResult.SetRequiredRows(required, e.maxChunkSize)
		}
		err := Next(ctx, e.probeSideExec, probeSideResult)
		if err != nil {
			e.joinResultCh <- &hashjoinWorkerResult{
				err: err,
			}
			return
		}
		if !hasWaitedForBuild {
			if probeSideResult.NumRows() == 0 {
				e.finished.Store(true)
				return
			}
			jobFinished, buildErr := e.wait4BuildSide()
			if buildErr != nil {
				e.joinResultCh <- &hashjoinWorkerResult{
					err: buildErr,
				}
				return
			} else if jobFinished {
				return
			}
			hasWaitedForBuild = true
		}

		if probeSideResult.NumRows() == 0 {
			return
		}

		probeSideResource.dest <- probeSideResult
	}
}

func (e *HashJoinExec) wait4BuildSide() (finished bool, err error) {
	select {
	case <-e.closeCh:
		return true, nil
	case err := <-e.buildFinished:
		if err != nil {
			return false, err
		}
	}
	if e.rowContainer.Len() == 0 && (e.joinType == plannercore.InnerJoin || e.joinType == plannercore.SemiJoin) {
		return true, nil
	}
	return false, nil
}

var buildSideResultLabel fmt.Stringer = stringutil.StringerStr("hashJoin.buildSideResult")

// fetchBuildSideRows fetches all rows from build side executor, and append them
// to e.buildSideResult.
func (e *HashJoinExec) fetchBuildSideRows(ctx context.Context, chkCh chan<- *chunk.Chunk, doneCh <-chan struct{}) {
	defer close(chkCh)
	var err error
	for {
		if e.finished.Load().(bool) {
			return
		}
		chk := chunk.NewChunkWithCapacity(e.buildSideExec.base().retFieldTypes, e.ctx.GetSessionVars().MaxChunkSize)
		err = Next(ctx, e.buildSideExec, chk)
		if err != nil {
			e.buildFinished <- errors.Trace(err)
			return
		}
		if chk.NumRows() == 0 {
			return
		}
		select {
		case <-doneCh:
			return
		case <-e.closeCh:
			return
		case chkCh <- chk:
		}
	}
}

func (e *HashJoinExec) initializeForProbe() {
	// e.probeResultChs is for transmitting the chunks which store the data of
	// probeSideExec, it'll be written by probe side worker goroutine, and read by join
	// workers.
	e.probeResultChs = make([]chan *chunk.Chunk, e.concurrency)
	for i := uint(0); i < e.concurrency; i++ {
		e.probeResultChs[i] = make(chan *chunk.Chunk, 1)
	}

	// e.probeChkResourceCh is for transmitting the used probeSideExec chunks from
	// join workers to probeSideExec worker.
	e.probeChkResourceCh = make(chan *probeChkResource, e.concurrency)
	for i := uint(0); i < e.concurrency; i++ {
		e.probeChkResourceCh <- &probeChkResource{
			chk:  newFirstChunk(e.probeSideExec),
			dest: e.probeResultChs[i],
		}
	}

	// e.joinChkResourceCh is for transmitting the reused join result chunks
	// from the main thread to join worker goroutines.
	e.joinChkResourceCh = make([]chan *chunk.Chunk, e.concurrency)
	for i := uint(0); i < e.concurrency; i++ {
		e.joinChkResourceCh[i] = make(chan *chunk.Chunk, 1)
		e.joinChkResourceCh[i] <- newFirstChunk(e)
	}

	// e.joinResultCh is for transmitting the join result chunks to the main
	// thread.
	e.joinResultCh = make(chan *hashjoinWorkerResult, e.concurrency+1)
}

func (e *HashJoinExec) fetchAndProbeHashTable(ctx context.Context) {
	e.initializeForProbe()
	e.joinWorkerWaitGroup.Add(1)
	go util.WithRecovery(func() { e.fetchProbeSideChunks(ctx) }, e.handleProbeSideFetcherPanic)

	probeKeyColIdx := make([]int, len(e.probeKeys))
	for i := range e.probeKeys {
		probeKeyColIdx[i] = e.probeKeys[i].Index
	}

	// Start e.concurrency join workers to probe hash table and join build side and
	// probe side rows.
	for i := uint(0); i < e.concurrency; i++ {
		e.joinWorkerWaitGroup.Add(1)
		workID := i
		go util.WithRecovery(func() { e.runJoinWorker(workID, probeKeyColIdx) }, e.handleJoinWorkerPanic)
	}
	go util.WithRecovery(e.waitJoinWorkersAndCloseResultChan, nil)
}

func (e *HashJoinExec) handleProbeSideFetcherPanic(r interface{}) {
	for i := range e.probeResultChs {
		close(e.probeResultChs[i])
	}
	if r != nil {
		e.joinResultCh <- &hashjoinWorkerResult{err: errors.Errorf("%v", r)}
	}
	e.joinWorkerWaitGroup.Done()
}

func (e *HashJoinExec) handleJoinWorkerPanic(r interface{}) {
	if r != nil {
		e.joinResultCh <- &hashjoinWorkerResult{err: errors.Errorf("%v", r)}
	}
	e.joinWorkerWaitGroup.Done()
}

func (e *HashJoinExec) waitJoinWorkersAndCloseResultChan() {
	e.joinWorkerWaitGroup.Wait()
	close(e.joinResultCh)
}

func (e *HashJoinExec) runJoinWorker(workerID uint, probeKeyColIdx []int) {
	var (
		probeSideResult *chunk.Chunk
		selected        = make([]bool, 0, chunk.InitialCapacity)
	)
	ok, joinResult := e.getNewJoinResult(workerID)
	if !ok {
		return
	}

	// Read and filter probeSideResult, and join the probeSideResult with the build side rows.
	emptyProbeSideResult := &probeChkResource{
		dest: e.probeResultChs[workerID],
	}
	hCtx := &hashContext{
		allTypes:  retTypes(e.probeSideExec),
		keyColIdx: probeKeyColIdx,
	}
	for ok := true; ok; {
		if e.finished.Load().(bool) {
			break
		}
		select {
		case <-e.closeCh:
			return
		case probeSideResult, ok = <-e.probeResultChs[workerID]:
		}
		if !ok {
			break
		}
		ok, joinResult = e.join2Chunk(workerID, probeSideResult, hCtx, joinResult, selected)
		if !ok {
			break
		}
		probeSideResult.Reset()
		emptyProbeSideResult.chk = probeSideResult
		e.probeChkResourceCh <- emptyProbeSideResult
	}
	if joinResult == nil {
		return
	} else if joinResult.err != nil || (joinResult.chk != nil && joinResult.chk.NumRows() > 0) {
		e.joinResultCh <- joinResult
	}
}

func (e *HashJoinExec) joinMatchedProbeSideRow2Chunk(workerID uint, probeKey uint64, probeSideRow chunk.Row, hCtx *hashContext,
	joinResult *hashjoinWorkerResult) (bool, *hashjoinWorkerResult) {
	buildSideRows, err := e.rowContainer.GetMatchedRows(probeKey, probeSideRow, hCtx)
	if err != nil {
		joinResult.err = err
		return false, joinResult
	}
	if len(buildSideRows) == 0 {
		e.joiners[workerID].onMissMatch(false, probeSideRow, joinResult.chk)
		return true, joinResult
	}
	iter := chunk.NewIterator4Slice(buildSideRows)
	hasMatch, hasNull := false, false
	for iter.Begin(); iter.Current() != iter.End(); {
		matched, isNull, err := e.joiners[workerID].tryToMatchInners(probeSideRow, iter, joinResult.chk)
		if err != nil {
			joinResult.err = err
			return false, joinResult
		}
		hasMatch = hasMatch || matched
		hasNull = hasNull || isNull

		if joinResult.chk.IsFull() {
			e.joinResultCh <- joinResult
			ok, joinResult := e.getNewJoinResult(workerID)
			if !ok {
				return false, joinResult
			}
		}
	}
	if !hasMatch {
		e.joiners[workerID].onMissMatch(hasNull, probeSideRow, joinResult.chk)
	}
	return true, joinResult
}

func (e *HashJoinExec) getNewJoinResult(workerID uint) (bool, *hashjoinWorkerResult) {
	joinResult := &hashjoinWorkerResult{
		src: e.joinChkResourceCh[workerID],
	}
	ok := true
	select {
	case <-e.closeCh:
		ok = false
	case joinResult.chk, ok = <-e.joinChkResourceCh[workerID]:
	}
	return ok, joinResult
}

func (e *HashJoinExec) join2Chunk(workerID uint, probeSideChk *chunk.Chunk, hCtx *hashContext, joinResult *hashjoinWorkerResult,
	selected []bool) (ok bool, _ *hashjoinWorkerResult) {
	var err error
	selected, err = expression.VectorizedFilter(e.ctx, e.probeSideFilter, chunk.NewIterator4Chunk(probeSideChk), selected)
	if err != nil {
		joinResult.err = err
		return false, joinResult
	}

	hCtx.initHash(probeSideChk.NumRows())
	for _, i := range hCtx.keyColIdx {
		err = codec.HashChunkSelected(e.rowContainer.sc, hCtx.hashVals, probeSideChk, hCtx.allTypes[i], i, hCtx.buf, hCtx.hasNull, selected)
		if err != nil {
			joinResult.err = err
			return false, joinResult
		}
	}

	for i := range selected {
		if !selected[i] || hCtx.hasNull[i] { // process unmatched probe side rows
			e.joiners[workerID].onMissMatch(false, probeSideChk.GetRow(i), joinResult.chk)
		} else { // process matched probe side rows
			probeKey, probeRow := hCtx.hashVals[i].Sum64(), probeSideChk.GetRow(i)
			ok, joinResult = e.joinMatchedProbeSideRow2Chunk(workerID, probeKey, probeRow, hCtx, joinResult)
			if !ok {
				return false, joinResult
			}
		}
		if joinResult.chk.IsFull() {
			e.joinResultCh <- joinResult
			ok, joinResult = e.getNewJoinResult(workerID)
			if !ok {
				return false, joinResult
			}
		}
	}
	return true, joinResult
}

// Next implements the Executor Next interface.
// hash join constructs the result following these steps:
// step 1. fetch data from build side child and build a hash table;
// step 2. fetch data from probe child in a background goroutine and probe the hash table in multiple join workers.
func (e *HashJoinExec) Next(ctx context.Context, req *chunk.Chunk) (err error) {
	if !e.prepared {
		e.buildFinished = make(chan error, 1)
		go util.WithRecovery(func() { e.fetchAndBuildHashTable(ctx) }, e.handleFetchAndBuildHashTablePanic)
		e.fetchAndProbeHashTable(ctx)
		e.prepared = true
	}
	if e.isOuterJoin {
		atomic.StoreInt64(&e.requiredRows, int64(req.RequiredRows()))
	}
	req.Reset()

	result, ok := <-e.joinResultCh
	if !ok {
		return nil
	}
	if result.err != nil {
		e.finished.Store(true)
		return result.err
	}
	req.SwapColumns(result.chk)
	result.src <- result.chk
	return nil
}

func (e *HashJoinExec) handleFetchAndBuildHashTablePanic(r interface{}) {
	if r != nil {
		e.buildFinished <- errors.Errorf("%v", r)
	}
	close(e.buildFinished)
}

func (e *HashJoinExec) fetchAndBuildHashTable(ctx context.Context) {
	// buildSideResultCh transfers build side chunk from build side fetch to build hash table.
	buildSideResultCh := make(chan *chunk.Chunk, 1)
	doneCh := make(chan struct{})
	go util.WithRecovery(func() { e.fetchBuildSideRows(ctx, buildSideResultCh, doneCh) }, nil)

	// TODO: Parallel build hash table. Currently not support because `rowHashMap` is not thread-safe.
	err := e.buildHashTableForList(buildSideResultCh)
	if err != nil {
		e.buildFinished <- errors.Trace(err)
		close(doneCh)
	}
	// Wait fetchBuildSideRows be finished.
	// 1. if buildHashTableForList fails
	// 2. if probeSideResult.NumRows() == 0, fetchProbeSideChunks will not wait for the build side.
	for range buildSideResultCh {
	}
}

// buildHashTableForList builds hash table from `list`.
func (e *HashJoinExec) buildHashTableForList(buildSideResultCh <-chan *chunk.Chunk) error {
	buildKeyColIdx := make([]int, len(e.buildKeys))
	for i := range e.buildKeys {
		buildKeyColIdx[i] = e.buildKeys[i].Index
	}
	allTypes := e.buildSideExec.base().retFieldTypes
	hCtx := &hashContext{
		allTypes:  allTypes,
		keyColIdx: buildKeyColIdx,
	}
	e.rowContainer = newHashRowContainer(e.ctx, int(e.buildSideEstCount), hCtx)
	e.rowContainer.GetMemTracker().AttachTo(e.memTracker)
	e.rowContainer.GetMemTracker().SetLabel(buildSideResultLabel)
	if config.GetGlobalConfig().OOMUseTmpStorage {
		actionSpill := e.rowContainer.ActionSpill()
		e.ctx.GetSessionVars().StmtCtx.MemTracker.FallbackOldAndSetNewAction(actionSpill)
	}
	for chk := range buildSideResultCh {
		if e.finished.Load().(bool) {
			return nil
		}
		err := e.rowContainer.PutChunk(chk)
		if err != nil {
			return err
		}
	}
	return nil
}

// NestedLoopApplyExec is the executor for apply.
type NestedLoopApplyExec struct {
	baseExecutor

	innerRows   []chunk.Row
	cursor      int
	innerExec   Executor
	outerExec   Executor
	innerFilter expression.CNFExprs
	outerFilter expression.CNFExprs
	outer       bool

	joiner joiner

	outerSchema []*expression.CorrelatedColumn

	outerChunk       *chunk.Chunk
	outerChunkCursor int
	outerSelected    []bool
	innerList        *chunk.ListInMemory
	innerChunk       *chunk.Chunk
	innerSelected    []bool
	innerIter        chunk.Iterator
	outerRow         *chunk.Row
	hasMatch         bool
	hasNull          bool

	memTracker *memory.Tracker // track memory usage.
}

// Close implements the Executor interface.
func (e *NestedLoopApplyExec) Close() error {
	e.innerRows = nil

	e.memTracker = nil
	return e.outerExec.Close()
}

var innerListLabel fmt.Stringer = stringutil.StringerStr("innerList")

// Open implements the Executor interface.
func (e *NestedLoopApplyExec) Open(ctx context.Context) error {
	err := e.outerExec.Open(ctx)
	if err != nil {
		return err
	}
	e.cursor = 0
	e.innerRows = e.innerRows[:0]
	e.outerChunk = newFirstChunk(e.outerExec)
	e.innerChunk = newFirstChunk(e.innerExec)
	e.innerList = chunk.NewListInMemory(retTypes(e.innerExec), e.initCap, e.maxChunkSize)

	e.memTracker = memory.NewTracker(e.id, e.ctx.GetSessionVars().MemQuotaNestedLoopApply)
	e.memTracker.AttachTo(e.ctx.GetSessionVars().StmtCtx.MemTracker)

	e.innerList.GetMemTracker().SetLabel(innerListLabel)
	e.innerList.GetMemTracker().AttachTo(e.memTracker)

	return nil
}

func (e *NestedLoopApplyExec) fetchSelectedOuterRow(ctx context.Context, chk *chunk.Chunk) (*chunk.Row, error) {
	outerIter := chunk.NewIterator4Chunk(e.outerChunk)
	for {
		if e.outerChunkCursor >= e.outerChunk.NumRows() {
			err := Next(ctx, e.outerExec, e.outerChunk)
			if err != nil {
				return nil, err
			}
			if e.outerChunk.NumRows() == 0 {
				return nil, nil
			}
			e.outerSelected, err = expression.VectorizedFilter(e.ctx, e.outerFilter, outerIter, e.outerSelected)
			if err != nil {
				return nil, err
			}
			e.outerChunkCursor = 0
		}
		outerRow := e.outerChunk.GetRow(e.outerChunkCursor)
		selected := e.outerSelected[e.outerChunkCursor]
		e.outerChunkCursor++
		if selected {
			return &outerRow, nil
		} else if e.outer {
			e.joiner.onMissMatch(false, outerRow, chk)
			if chk.IsFull() {
				return nil, nil
			}
		}
	}
}

// fetchAllInners reads all data from the inner table and stores them in a ListInMemory.
func (e *NestedLoopApplyExec) fetchAllInners(ctx context.Context) error {
	err := e.innerExec.Open(ctx)
	defer terror.Call(e.innerExec.Close)
	if err != nil {
		return err
	}
	e.innerList.Reset()
	innerIter := chunk.NewIterator4Chunk(e.innerChunk)
	for {
		err := Next(ctx, e.innerExec, e.innerChunk)
		if err != nil {
			return err
		}
		if e.innerChunk.NumRows() == 0 {
			return nil
		}

		e.innerSelected, err = expression.VectorizedFilter(e.ctx, e.innerFilter, innerIter, e.innerSelected)
		if err != nil {
			return err
		}
		for row := innerIter.Begin(); row != innerIter.End(); row = innerIter.Next() {
			if e.innerSelected[row.Idx()] {
				e.innerList.AppendRow(row)
			}
		}
	}
}

// Next implements the Executor interface.
func (e *NestedLoopApplyExec) Next(ctx context.Context, req *chunk.Chunk) (err error) {
	req.Reset()
	for {
		if e.innerIter == nil || e.innerIter.Current() == e.innerIter.End() {
			if e.outerRow != nil && !e.hasMatch {
				e.joiner.onMissMatch(e.hasNull, *e.outerRow, req)
			}
			e.outerRow, err = e.fetchSelectedOuterRow(ctx, req)
			if e.outerRow == nil || err != nil {
				return err
			}
			e.hasMatch = false
			e.hasNull = false

			for _, col := range e.outerSchema {
				*col.Data = e.outerRow.GetDatum(col.Index, col.RetType)
			}
			err = e.fetchAllInners(ctx)
			if err != nil {
				return err
			}
			e.innerIter = chunk.NewIterator4List(e.innerList)
			e.innerIter.Begin()
		}

		matched, isNull, err := e.joiner.tryToMatchInners(*e.outerRow, e.innerIter, req)
		e.hasMatch = e.hasMatch || matched
		e.hasNull = e.hasNull || isNull

		if err != nil || req.IsFull() {
			return err
		}
	}
}
