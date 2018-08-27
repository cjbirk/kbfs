// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/keybase/client/go/logger"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/kbfs/kbfsblock"
	"github.com/keybase/kbfs/kbfscodec"
	"github.com/keybase/kbfs/tlf"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

type overallBlockState int

const (
	// cleanState: no outstanding local writes.
	cleanState overallBlockState = iota
	// dirtyState: there are outstanding local writes that haven't yet been
	// synced.
	dirtyState
)

// blockReqType indicates whether an operation makes block
// modifications or not
type blockReqType int

const (
	// A block read request.
	blockRead blockReqType = iota
	// A block write request.
	blockWrite
	// A block read request that is happening from a different
	// goroutine than the blockLock rlock holder, using the same lState.
	blockReadParallel
	// We are looking up a block for the purposes of creating a new
	// node in the node cache for it; avoid any unlocks as part of the
	// lookup process.
	blockLookup
)

const (
	// numBlockSizeWorkersMax is the max number of workers to use when
	// fetching a set of block sizes.
	numBlockSizeWorkersMax = 50
	// truncateExtendCutoffPoint is the amount of data in extending
	// truncate that will trigger the extending with a hole algorithm.
	truncateExtendCutoffPoint = 128 * 1024
)

type mdToCleanIfUnused struct {
	md  ReadOnlyRootMetadata
	bps *blockPutState
}

type syncInfo struct {
	oldInfo         BlockInfo
	op              *syncOp
	unrefs          []BlockInfo
	bps             *blockPutState
	refBytes        uint64
	unrefBytes      uint64
	toCleanIfUnused []mdToCleanIfUnused
}

func (si *syncInfo) DeepCopy(codec kbfscodec.Codec) (*syncInfo, error) {
	newSi := &syncInfo{
		oldInfo:    si.oldInfo,
		refBytes:   si.refBytes,
		unrefBytes: si.unrefBytes,
	}
	newSi.unrefs = make([]BlockInfo, len(si.unrefs))
	copy(newSi.unrefs, si.unrefs)
	if si.bps != nil {
		newSi.bps = si.bps.DeepCopy()
	}
	if si.op != nil {
		err := kbfscodec.Update(codec, &newSi.op, si.op)
		if err != nil {
			return nil, err
		}
	}
	newSi.toCleanIfUnused = make([]mdToCleanIfUnused, len(si.toCleanIfUnused))
	for i, toClean := range si.toCleanIfUnused {
		// It might be overkill to deep-copy these MDs and bpses,
		// which are probably immutable, but for now let's do the safe
		// thing.
		copyMd, err := toClean.md.deepCopy(codec)
		if err != nil {
			return nil, err
		}
		newSi.toCleanIfUnused[i].md = copyMd.ReadOnly()
		newSi.toCleanIfUnused[i].bps = toClean.bps.DeepCopy()
	}
	return newSi, nil
}

func (si *syncInfo) removeReplacedBlock(ctx context.Context,
	log logger.Logger, ptr BlockPointer) {
	for i, ref := range si.op.RefBlocks {
		if ref == ptr {
			log.CDebugf(ctx, "Replacing old ref %v", ptr)
			si.op.RefBlocks = append(si.op.RefBlocks[:i],
				si.op.RefBlocks[i+1:]...)
			for j, unref := range si.unrefs {
				if unref.BlockPointer == ptr {
					si.unrefs = append(si.unrefs[:j], si.unrefs[j+1:]...)
				}
			}
			break
		}
	}
}

func (si *syncInfo) mergeUnrefCache(md *RootMetadata) {
	for _, info := range si.unrefs {
		// it's ok if we push the same ptr.ID/RefNonce multiple times,
		// because the subsequent ones should have a QuotaSize of 0.
		md.AddUnrefBlock(info)
	}
}

type deferredState struct {
	// Writes and truncates for blocks that were being sync'd, and
	// need to be replayed after the sync finishes on top of the new
	// versions of the blocks.
	writes []func(context.Context, *lockState, KeyMetadataWithRootDirEntry, path) error
	// Blocks that need to be deleted from the dirty cache before any
	// deferred writes are replayed.
	dirtyDeletes []BlockPointer
	waitBytes    int64
}

// folderBlockOps contains all the fields that must be synchronized by
// blockLock. It will eventually also contain all the methods that
// must be synchronized by blockLock, so that folderBranchOps will
// have no knowledge of blockLock.
//
// -- And now, a primer on tracking dirty bytes --
//
// The DirtyBlockCache tracks the number of bytes that are dirtied
// system-wide, as the number of bytes that haven't yet been synced
// ("unsynced"), and a number of bytes that haven't yet been resolved
// yet because the overall file Sync hasn't finished yet ("total").
// This data helps us decide when we need to block incoming Writes, in
// order to keep memory usage from exploding.
//
// It's the responsibility of folderBlockOps (and its helper struct
// dirtyFile) to update these totals in DirtyBlockCache for the
// individual files within this TLF.  This is complicated by a few things:
//   * New writes to a file are "deferred" while a Sync is happening, and
//     are replayed after the Sync finishes.
//   * Syncs can be canceled or error out halfway through syncing the blocks,
//     leaving the file in a dirty state until the next Sync.
//   * Syncs can fail with a /recoverable/ error, in which case they get
//     retried automatically by folderBranchOps.  In that case, the retried
//     Sync also sucks in any outstanding deferred writes.
//
// With all that in mind, here is the rough breakdown of how this
// bytes-tracking is implemented:
//   * On a Write/Truncate to a block, folderBranchOps counts all the
//     newly-dirtied bytes in a file as "unsynced".  That is, if the block was
//     already in the dirty cache (and not already being synced), only
//     extensions to the block count as "unsynced" bytes.
//   * When a Sync starts, dirtyFile remembers the total of bytes being synced,
//     and the size of each block being synced.
//   * When each block put finishes successfully, dirtyFile subtracts the size
//     of that block from "unsynced".
//   * When a Sync finishes successfully, the total sum of bytes in that sync
//     are subtracted from the "total" dirty bytes outstanding.
//   * If a Sync fails, but some blocks were put successfully, those blocks
//     are "re-dirtied", which means they count as unsynced bytes again.
//     dirtyFile handles this.
//   * When a Write/Truncate is deferred due to an ongoing Sync, its bytes
//     still count towards the "unsynced" total.  In fact, this essentially
//     creates a new copy of those blocks, and the whole size of that block
//     (not just the newly-dirtied bytes) count for the total.  However,
//     when the write gets replayed, folderBlockOps first subtracts those bytes
//     from the system-wide numbers, since they are about to be replayed.
//   * When a Sync is retried after a recoverable failure, dirtyFile adds
//     the newly-dirtied deferred bytes to the system-wide numbers, since they
//     are now being assimilated into this Sync.
//   * dirtyFile also exposes a concept of "orphaned" blocks.  These are child
//     blocks being synced that are now referenced via a new, permanent block
//     ID from the parent indirect block.  This matters for when hard failures
//     occur during a Sync -- the blocks will no longer be accessible under
//     their previous old pointers, and so dirtyFile needs to know their old
//     bytes can be cleaned up now.
type folderBlockOps struct {
	config       Config
	log          logger.Logger
	folderBranch FolderBranch
	observers    *observerList

	// forceSyncChan can be sent on to trigger an immediate
	// Sync().  It is a blocking channel.
	forceSyncChan chan<- struct{}

	// protects access to blocks in this folder and all fields
	// below.
	blockLock blockLock

	// Which files are currently dirty and have dirty blocks that are either
	// currently syncing, or waiting to be sync'd.
	dirtyFiles map[BlockPointer]*dirtyFile

	// For writes and truncates, track the unsynced to-be-unref'd
	// block infos, per-path.
	unrefCache map[BlockRef]*syncInfo

	// dirtyDirs track which directories are currently dirty in this
	// TLF.
	dirtyDirs map[BlockPointer][]BlockInfo

	// dirtyRootDirEntry is a DirEntry representing the root of the
	// TLF (to be copied into the RootMetadata on a sync).
	dirtyRootDirEntry *DirEntry

	chargedTo keybase1.UserOrTeamID

	// Track deferred operations on a per-file basis.
	deferred map[BlockRef]deferredState

	// set to true if this write or truncate should be deferred
	doDeferWrite bool

	// nodeCache itself is goroutine-safe, but write/truncate must
	// call PathFromNode() only under blockLock (see nodeCache
	// comments in folder_branch_ops.go).
	nodeCache NodeCache
}

// Only exported methods of folderBlockOps should be used outside of this
// file.
//
// Although, temporarily, folderBranchOps is allowed to reach in and
// manipulate folderBlockOps fields and methods directly.

func (fbo *folderBlockOps) id() tlf.ID {
	return fbo.folderBranch.Tlf
}

func (fbo *folderBlockOps) branch() BranchName {
	return fbo.folderBranch.Branch
}

// GetState returns the overall block state of this TLF.
func (fbo *folderBlockOps) GetState(lState *lockState) overallBlockState {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	if len(fbo.dirtyFiles) == 0 && len(fbo.dirtyDirs) == 0 &&
		fbo.dirtyRootDirEntry == nil {
		return cleanState
	}
	return dirtyState
}

// getCleanEncodedBlockHelperLocked retrieves the encoded size of the
// clean block pointed to by ptr, which must be valid, either from the
// cache or from the server.  If `rtype` is `blockReadParallel`, it's
// assumed that some coordinating goroutine is holding the correct
// locks, and in that case `lState` must be `nil`.
func (fbo *folderBlockOps) getCleanEncodedBlockSizeLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer, branch BranchName,
	rtype blockReqType) (uint32, error) {
	if rtype != blockReadParallel {
		if rtype == blockWrite {
			panic("Cannot get the size of a block for writing")
		}
		fbo.blockLock.AssertAnyLocked(lState)
	} else if lState != nil {
		panic("Non-nil lState passed to getCleanEncodedBlockSizeLocked " +
			"with blockReadParallel")
	}

	if !ptr.IsValid() {
		return 0, InvalidBlockRefError{ptr.Ref()}
	}

	// Try to get the encoded size from the cache before escalating to
	// the block retriever (even though it's supposed to do a similar
	// thing with checking caches before checking the data version).
	// TODO: Figure out how to remove this without breaking journal
	// tests in kbfs/test.
	if block, err := fbo.config.BlockCache().Get(ptr); err == nil {
		return block.GetEncodedSize(), nil
	}

	if err := checkDataVersion(fbo.config, path{}, ptr); err != nil {
		return 0, err
	}

	// Unlock the blockLock while we wait for the network, only if
	// it's locked for reading by a single goroutine.  If it's locked
	// for writing, that indicates we are performing an atomic write
	// operation, and we need to ensure that nothing else comes in and
	// modifies the blocks, so don't unlock.
	//
	// If there may be multiple goroutines fetching blocks under the
	// same lState, we can't safely unlock since some of the other
	// goroutines may be operating on the data assuming they have the
	// lock.
	bops := fbo.config.BlockOps()
	var size uint32
	var err error
	if rtype != blockReadParallel && rtype != blockLookup {
		fbo.blockLock.DoRUnlockedIfPossible(lState, func(*lockState) {
			size, _, err = bops.GetEncodedSize(ctx, kmd, ptr)
		})
	} else {
		size, _, err = bops.GetEncodedSize(ctx, kmd, ptr)
	}
	if err != nil {
		return 0, err
	}

	return size, nil
}

// getBlockHelperLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. If
// notifyPath is valid and the block isn't cached, trigger a read
// notification.  If `rtype` is `blockReadParallel`, it's assumed that
// some coordinating goroutine is holding the correct locks, and
// in that case `lState` must be `nil`.
//
// This must be called only by get{File,Dir}BlockHelperLocked().
func (fbo *folderBlockOps) getBlockHelperLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer, branch BranchName,
	newBlock makeNewBlock, lifetime BlockCacheLifetime, notifyPath path,
	rtype blockReqType) (Block, error) {
	if rtype != blockReadParallel {
		fbo.blockLock.AssertAnyLocked(lState)
	} else if lState != nil {
		panic("Non-nil lState passed to getBlockHelperLocked " +
			"with blockReadParallel")
	}

	if !ptr.IsValid() {
		return nil, InvalidBlockRefError{ptr.Ref()}
	}

	if block, err := fbo.config.DirtyBlockCache().Get(
		fbo.id(), ptr, branch); err == nil {
		return block, nil
	}

	if block, prefetchStatus, lifetime, err :=
		fbo.config.BlockCache().GetWithPrefetch(ptr); err == nil {
		// If the block was cached in the past, we need to handle it as if it's
		// an on-demand request so that its downstream prefetches are triggered
		// correctly according to the new on-demand fetch priority.
		fbo.config.BlockOps().Prefetcher().ProcessBlockForPrefetch(ctx, ptr,
			block, kmd, defaultOnDemandRequestPriority, lifetime,
			prefetchStatus)
		return block, nil
	}

	if err := checkDataVersion(fbo.config, notifyPath, ptr); err != nil {
		return nil, err
	}

	if notifyPath.isValidForNotification() {
		fbo.config.Reporter().Notify(ctx, readNotification(notifyPath, false))
		defer fbo.config.Reporter().Notify(ctx,
			readNotification(notifyPath, true))
	}

	// Unlock the blockLock while we wait for the network, only if
	// it's locked for reading by a single goroutine.  If it's locked
	// for writing, that indicates we are performing an atomic write
	// operation, and we need to ensure that nothing else comes in and
	// modifies the blocks, so don't unlock.
	//
	// If there may be multiple goroutines fetching blocks under the
	// same lState, we can't safely unlock since some of the other
	// goroutines may be operating on the data assuming they have the
	// lock.
	// fetch the block, and add to cache
	block := newBlock()
	bops := fbo.config.BlockOps()
	var err error
	if rtype != blockReadParallel && rtype != blockLookup {
		fbo.blockLock.DoRUnlockedIfPossible(lState, func(*lockState) {
			err = bops.Get(ctx, kmd, ptr, block, lifetime)
		})
	} else {
		err = bops.Get(ctx, kmd, ptr, block, lifetime)
	}
	if err != nil {
		return nil, err
	}

	return block, nil
}

// getFileBlockHelperLocked retrieves the block pointed to by ptr,
// which must be valid, either from an internal cache, the block
// cache, or from the server. An error is returned if the retrieved
// block is not a file block.  If `rtype` is `blockReadParallel`, it's
// assumed that some coordinating goroutine is holding the correct
// locks, and in that case `lState` must be `nil`.
//
// This must be called only by GetFileBlockForReading(),
// getFileBlockLocked(), and getFileLocked().
//
// p is used only when reporting errors and sending read
// notifications, and can be empty.
func (fbo *folderBlockOps) getFileBlockHelperLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer,
	branch BranchName, p path, rtype blockReqType) (
	*FileBlock, error) {
	if rtype != blockReadParallel {
		fbo.blockLock.AssertAnyLocked(lState)
	} else if lState != nil {
		panic("Non-nil lState passed to getFileBlockHelperLocked " +
			"with blockReadParallel")
	}

	block, err := fbo.getBlockHelperLocked(
		ctx, lState, kmd, ptr, branch, NewFileBlock, TransientEntry, p, rtype)
	if err != nil {
		return nil, err
	}

	fblock, ok := block.(*FileBlock)
	if !ok {
		return nil, NotFileBlockError{ptr, branch, p}
	}

	return fblock, nil
}

// GetBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server.  The
// returned block may have a generic type (not DirBlock or FileBlock).
//
// This should be called for "internal" operations, like conflict
// resolution and state checking, which don't know what kind of block
// the pointer refers to.  The block will not be cached, if it wasn't
// in the cache already.
func (fbo *folderBlockOps) GetBlockForReading(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer, branch BranchName) (
	Block, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getBlockHelperLocked(ctx, lState, kmd, ptr, branch,
		NewCommonBlock, NoCacheEntry, path{}, blockRead)
}

// GetCleanEncodedBlocksSizeSum retrieves the sum of the encoded sizes
// of the blocks pointed to by ptrs, all of which must be valid,
// either from the cache or from the server.
//
// The caller can specify a set of pointers using
// `ignoreRecoverableForRemovalErrors` for which "recoverable" fetch
// errors are tolerated.  In that case, the returned sum will not
// include the size for any pointers in the
// `ignoreRecoverableForRemovalErrors` set that hit such an error.
//
// This should be called for "internal" operations, like conflict
// resolution and state checking, which don't know what kind of block
// the pointers refer to.  Any downloaded blocks will not be cached,
// if they weren't in the cache already.
func (fbo *folderBlockOps) GetCleanEncodedBlocksSizeSum(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptrs []BlockPointer,
	ignoreRecoverableForRemovalErrors map[BlockPointer]bool,
	branch BranchName) (uint64, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	ptrCh := make(chan BlockPointer, len(ptrs))
	sumCh := make(chan uint32, len(ptrs))
	eg, groupCtx := errgroup.WithContext(ctx)
	for _, ptr := range ptrs {
		ptrCh <- ptr
	}

	numWorkers := numBlockSizeWorkersMax
	if len(ptrs) < numWorkers {
		numWorkers = len(ptrs)
	}

	for i := 0; i < numWorkers; i++ {
		eg.Go(func() error {
			for ptr := range ptrCh {
				size, err := fbo.getCleanEncodedBlockSizeLocked(groupCtx, nil,
					kmd, ptr, branch, blockReadParallel)
				// TODO: we might be able to recover the size of the
				// top-most block of a removed file using the merged
				// directory entry, the same way we do in
				// `folderBranchOps.unrefEntry`.
				if isRecoverableBlockErrorForRemoval(err) &&
					ignoreRecoverableForRemovalErrors[ptr] {
					fbo.log.CDebugf(groupCtx, "Hit an ignorable, recoverable "+
						"error for block %v: %v", ptr, err)
					continue
				}

				if err != nil {
					return err
				}
				sumCh <- size
			}
			return nil
		})
	}
	close(ptrCh)

	if err := eg.Wait(); err != nil {
		return 0, err
	}
	close(sumCh)

	var sum uint64
	for size := range sumCh {
		sum += uint64(size)
	}
	return sum, nil
}

// getDirBlockHelperLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a dir block.
//
// This must be called only by GetDirBlockForReading() and
// getDirLocked().
//
// p is used only when reporting errors, and can be empty.
func (fbo *folderBlockOps) getDirBlockHelperLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer,
	branch BranchName, p path, rtype blockReqType) (*DirBlock, error) {
	if rtype != blockReadParallel {
		fbo.blockLock.AssertAnyLocked(lState)
	}

	// Pass in an empty notify path because notifications should only
	// trigger for file reads.
	block, err := fbo.getBlockHelperLocked(
		ctx, lState, kmd, ptr, branch, NewDirBlock, TransientEntry, path{}, rtype)
	if err != nil {
		return nil, err
	}

	dblock, ok := block.(*DirBlock)
	if !ok {
		return nil, NotDirBlockError{ptr, branch, p}
	}

	return dblock, nil
}

// GetFileBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a file block.
//
// This should be called for "internal" operations, like conflict
// resolution and state checking. "Real" operations should use
// getFileBlockLocked() and getFileLocked() instead.
//
// p is used only when reporting errors, and can be empty.
func (fbo *folderBlockOps) GetFileBlockForReading(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer,
	branch BranchName, p path) (*FileBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getFileBlockHelperLocked(
		ctx, lState, kmd, ptr, branch, p, blockRead)
}

// GetDirBlockForReading retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a dir block.
//
// This should be called for "internal" operations, like conflict
// resolution and state checking. "Real" operations should use
// getDirLocked() instead.
//
// p is used only when reporting errors, and can be empty.
func (fbo *folderBlockOps) GetDirBlockForReading(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer,
	branch BranchName, p path) (*DirBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getDirBlockHelperLocked(
		ctx, lState, kmd, ptr, branch, p, blockRead)
}

// getFileBlockLocked retrieves the block pointed to by ptr, which
// must be valid, either from the cache or from the server. An error
// is returned if the retrieved block is not a file block.
//
// The given path must be valid, and the given pointer must be its
// tail pointer or an indirect pointer from it. A read notification is
// triggered for the given path only if the block isn't in the cache.
//
// This shouldn't be called for "internal" operations, like conflict
// resolution and state checking -- use GetFileBlockForReading() for
// those instead.
//
// When rtype == blockWrite and the cached version of the block is
// currently clean, or the block is currently being synced, this
// method makes a copy of the file block and returns it.  If this
// method might be called again for the same block within a single
// operation, it is the caller's responsibility to write that block
// back to the cache as dirty.
//
// Note that blockLock must be locked exactly when rtype ==
// blockWrite, and must be r-locked when rtype == blockRead.  (This
// differs from getDirLocked.)  This is because a write operation
// (like write, truncate and sync which lock blockLock) fetching a
// file block will almost always need to modify that block, and so
// will pass in blockWrite.  If rtype == blockReadParallel, it's
// assumed that some coordinating goroutine is holding the correct
// locks, and in that case `lState` must be `nil`.
//
// file is used only when reporting errors and sending read
// notifications, and can be empty except that file.Branch must be set
// correctly.
//
// This method also returns whether the block was already dirty.
func (fbo *folderBlockOps) getFileBlockLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer,
	file path, rtype blockReqType) (
	fblock *FileBlock, wasDirty bool, err error) {
	switch rtype {
	case blockRead:
		fbo.blockLock.AssertRLocked(lState)
	case blockWrite:
		fbo.blockLock.AssertLocked(lState)
	case blockReadParallel:
		// This goroutine might not be the official lock holder, so
		// don't make any assertions.
		if lState != nil {
			panic("Non-nil lState passed to getFileBlockLocked " +
				"with blockReadParallel")
		}
	case blockLookup:
		panic("blockLookup should only be used for directory blocks")
	default:
		panic(fmt.Sprintf("Unknown block req type: %d", rtype))
	}

	fblock, err = fbo.getFileBlockHelperLocked(
		ctx, lState, kmd, ptr, file.Branch, file, rtype)
	if err != nil {
		return nil, false, err
	}

	wasDirty = fbo.config.DirtyBlockCache().IsDirty(fbo.id(), ptr, file.Branch)
	if rtype == blockWrite {
		// Copy the block if it's for writing, and either the
		// block is not yet dirty or the block is currently
		// being sync'd and needs a copy even though it's
		// already dirty.
		df := fbo.dirtyFiles[file.tailPointer()]
		if !wasDirty || (df != nil && df.blockNeedsCopy(ptr)) {
			fblock = fblock.DeepCopy()
		}
	}
	return fblock, wasDirty, nil
}

// getFileLocked is getFileBlockLocked called with file.tailPointer().
func (fbo *folderBlockOps) getFileLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadata, file path,
	rtype blockReqType) (*FileBlock, error) {
	// Callers should have already done this check, but it doesn't
	// hurt to do it again.
	if !file.isValid() {
		return nil, errors.WithStack(InvalidPathError{file})
	}
	fblock, _, err := fbo.getFileBlockLocked(
		ctx, lState, kmd, file.tailPointer(), file, rtype)
	return fblock, err
}

func (fbo *folderBlockOps) getIndirectFileBlockInfosLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadata, file path) (
	[]BlockInfo, error) {
	fbo.blockLock.AssertRLocked(lState)
	var id keybase1.UserOrTeamID // Data reads don't depend on the id.
	fd := fbo.newFileData(lState, file, id, kmd)
	return fd.getIndirectFileBlockInfos(ctx)
}

// GetIndirectFileBlockInfos returns a list of BlockInfos for all
// indirect blocks of the given file. If the returned error is a
// recoverable one (as determined by
// isRecoverableBlockErrorForRemoval), the returned list may still be
// non-empty, and holds all the BlockInfos for all found indirect
// blocks.
func (fbo *folderBlockOps) GetIndirectFileBlockInfos(ctx context.Context,
	lState *lockState, kmd KeyMetadata, file path) ([]BlockInfo, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getIndirectFileBlockInfosLocked(ctx, lState, kmd, file)
}

// GetIndirectDirBlockInfos returns a list of BlockInfos for all
// indirect blocks of the given directory. If the returned error is a
// recoverable one (as determined by
// isRecoverableBlockErrorForRemoval), the returned list may still be
// non-empty, and holds all the BlockInfos for all found indirect
// blocks.
func (fbo *folderBlockOps) GetIndirectDirBlockInfos(
	ctx context.Context, lState *lockState, kmd KeyMetadata, dir path) (
	[]BlockInfo, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	var id keybase1.UserOrTeamID // Data reads don't depend on the id.
	fd := fbo.newDirDataLocked(lState, dir, id, kmd)
	return fd.getIndirectDirBlockInfos(ctx)
}

// GetIndirectFileBlockInfosWithTopBlock returns a list of BlockInfos
// for all indirect blocks of the given file, starting from the given
// top-most block. If the returned error is a recoverable one (as
// determined by isRecoverableBlockErrorForRemoval), the returned list
// may still be non-empty, and holds all the BlockInfos for all found
// indirect blocks. (This will be relevant when we handle multiple
// levels of indirection.)
func (fbo *folderBlockOps) GetIndirectFileBlockInfosWithTopBlock(
	ctx context.Context, lState *lockState, kmd KeyMetadata, file path,
	topBlock *FileBlock) (
	[]BlockInfo, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	var id keybase1.UserOrTeamID // Data reads don't depend on the id.
	fd := fbo.newFileData(lState, file, id, kmd)
	return fd.getIndirectFileBlockInfosWithTopBlock(ctx, topBlock)
}

func (fbo *folderBlockOps) getChargedToLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadata) (
	keybase1.UserOrTeamID, error) {
	fbo.blockLock.AssertAnyLocked(lState)
	if !fbo.chargedTo.IsNil() {
		return fbo.chargedTo, nil
	}
	chargedTo, err := chargedToForTLF(
		ctx, fbo.config.KBPKI(), fbo.config.KBPKI(), kmd.GetTlfHandle())
	if err != nil {
		return keybase1.UserOrTeamID(""), err
	}
	fbo.chargedTo = chargedTo
	return chargedTo, nil
}

// ClearChargedTo clears out the cached chargedTo UID for this FBO.
func (fbo *folderBlockOps) ClearChargedTo(lState *lockState) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	fbo.chargedTo = keybase1.UserOrTeamID("")
}

// DeepCopyFile makes a complete copy of the given file, deduping leaf
// blocks and making new random BlockPointers for all indirect blocks.
// It returns the new top pointer of the copy, and all the new child
// pointers in the copy.  It takes a custom DirtyBlockCache, which
// directs where the resulting block copies are stored.
func (fbo *folderBlockOps) deepCopyFileLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadata, file path,
	dirtyBcache DirtyBlockCache, dataVer DataVer) (
	newTopPtr BlockPointer, allChildPtrs []BlockPointer, err error) {
	// Deep copying doesn't alter any data in use, it only makes copy,
	// so only a read lock is needed.
	fbo.blockLock.AssertRLocked(lState)
	chargedTo, err := chargedToForTLF(
		ctx, fbo.config.KBPKI(), fbo.config.KBPKI(), kmd.GetTlfHandle())
	if err != nil {
		return BlockPointer{}, nil, err
	}
	fd := fbo.newFileDataWithCache(
		lState, file, chargedTo, kmd, dirtyBcache)
	return fd.deepCopy(ctx, dataVer)
}

func (fbo *folderBlockOps) UndupChildrenInCopy(ctx context.Context,
	lState *lockState, kmd KeyMetadata, file path, bps *blockPutState,
	dirtyBcache DirtyBlockCache, topBlock *FileBlock) ([]BlockInfo, error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return nil, err
	}
	fd := fbo.newFileDataWithCache(
		lState, file, chargedTo, kmd, dirtyBcache)
	return fd.undupChildrenInCopy(ctx, fbo.config.BlockCache(),
		fbo.config.BlockOps(), bps, topBlock)
}

func (fbo *folderBlockOps) ReadyNonLeafBlocksInCopy(ctx context.Context,
	lState *lockState, kmd KeyMetadata, file path, bps *blockPutState,
	dirtyBcache DirtyBlockCache, topBlock *FileBlock) ([]BlockInfo, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return nil, err
	}

	fd := fbo.newFileDataWithCache(
		lState, file, chargedTo, kmd, dirtyBcache)
	return fd.readyNonLeafBlocksInCopy(ctx, fbo.config.BlockCache(),
		fbo.config.BlockOps(), bps, topBlock)
}

// getDirLocked retrieves the block pointed to by the tail pointer of
// the given path, which must be valid, either from the cache or from
// the server. An error is returned if the retrieved block is not a
// dir block.
//
// This shouldn't be called for "internal" operations, like conflict
// resolution and state checking -- use GetDirBlockForReading() for
// those instead.
//
// When rtype == blockWrite and the cached version of the block is
// currently clean, this method makes a copy of the directory block
// and returns it.  If this method might be called again for the same
// block within a single operation, it is the caller's responsibility
// to write that block back to the cache as dirty.
//
// Note that blockLock must be either r-locked or locked, but
// independently of rtype. (This differs from getFileLocked and
// getFileBlockLocked.) File write operations (which lock blockLock)
// don't need a copy of parent dir blocks, and non-file write
// operations do need to copy dir blocks for modifications.
func (fbo *folderBlockOps) getDirLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadata, ptr BlockPointer, dir path,
	rtype blockReqType) (*DirBlock, bool, error) {
	switch rtype {
	case blockRead, blockWrite, blockLookup:
		fbo.blockLock.AssertAnyLocked(lState)
	case blockReadParallel:
		// This goroutine might not be the official lock holder, so
		// don't make any assertions.
		if lState != nil {
			panic("Non-nil lState passed to getFileBlockLocked " +
				"with blockReadParallel")
		}
	default:
		panic(fmt.Sprintf("Unknown block req type: %d", rtype))
	}

	// Callers should have already done this check, but it doesn't
	// hurt to do it again.
	if !dir.isValid() {
		return nil, false, errors.WithStack(InvalidPathError{dir})
	}

	// Get the block for the last element in the path.
	dblock, err := fbo.getDirBlockHelperLocked(
		ctx, lState, kmd, ptr, dir.Branch, dir, rtype)
	if err != nil {
		return nil, false, err
	}

	wasDirty := fbo.config.DirtyBlockCache().IsDirty(fbo.id(), ptr, dir.Branch)
	if rtype == blockWrite && !wasDirty {
		// Copy the block if it's for writing and the block is
		// not yet dirty.
		dblock = dblock.DeepCopy()
	}
	return dblock, wasDirty, nil
}

// GetDir retrieves the block pointed to by the tail pointer of the
// given path, which must be valid, either from the cache or from the
// server. An error is returned if the retrieved block is not a dir
// block.
//
// This shouldn't be called for "internal" operations, like conflict
// resolution and state checking -- use GetDirBlockForReading() for
// those instead.
//
// When rtype == blockWrite and the cached version of the block is
// currently clean, this method makes a copy of the directory block
// and returns it.  If this method might be called again for the same
// block within a single operation, it is the caller's responsibility
// to write that block back to the cache as dirty.
func (fbo *folderBlockOps) GetDir(
	ctx context.Context, lState *lockState, kmd KeyMetadata, dir path,
	rtype blockReqType) (*DirBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	dblock, _, err := fbo.getDirLocked(
		ctx, lState, kmd, dir.tailPointer(), dir, rtype)
	return dblock, err
}

type dirCacheUndoFn func(lState *lockState)

func (fbo *folderBlockOps) wrapWithBlockLock(fn func()) dirCacheUndoFn {
	return func(lState *lockState) {
		if fn == nil {
			return
		}
		fbo.blockLock.Lock(lState)
		defer fbo.blockLock.Unlock(lState)
		fn()
	}
}

func (fbo *folderBlockOps) newDirDataLocked(lState *lockState,
	dir path, chargedTo keybase1.UserOrTeamID, kmd KeyMetadata) *dirData {
	fbo.blockLock.AssertAnyLocked(lState)
	return newDirData(dir, chargedTo, fbo.config.Crypto(),
		fbo.config.BlockSplitter(), kmd,
		func(ctx context.Context, kmd KeyMetadata, ptr BlockPointer,
			dir path, rtype blockReqType) (*DirBlock, bool, error) {
			lState := lState
			if rtype == blockReadParallel {
				lState = nil
			}
			return fbo.getDirLocked(
				ctx, lState, kmd, ptr, dir, rtype)
		},
		func(ptr BlockPointer, block Block) error {
			return fbo.config.DirtyBlockCache().Put(
				fbo.id(), ptr, dir.Branch, block)
		}, fbo.log)
}

// newDirDataWithLBCLocked creates a new `dirData` that reads from and
// puts into a local block cache.  If it reads a block out from
// anything but the `lbc`, it makes a copy of it before inserting it
// into the `lbc`.
func (fbo *folderBlockOps) newDirDataWithLBCLocked(lState *lockState,
	dir path, chargedTo keybase1.UserOrTeamID, kmd KeyMetadata,
	lbc localBcache) *dirData {
	fbo.blockLock.AssertRLocked(lState)
	return newDirData(dir, chargedTo, fbo.config.Crypto(),
		fbo.config.BlockSplitter(), kmd,
		func(ctx context.Context, kmd KeyMetadata, ptr BlockPointer,
			dir path, rtype blockReqType) (*DirBlock, bool, error) {
			block, ok := lbc[ptr]
			if ok {
				return block, true, nil
			}

			lState := lState
			getRtype := rtype
			switch rtype {
			case blockReadParallel:
				lState = nil
			case blockWrite:
				getRtype = blockRead
			}

			block, wasDirty, err := fbo.getDirLocked(
				ctx, lState, kmd, ptr, dir, getRtype)
			if err != nil {
				return nil, false, err
			}

			if rtype == blockWrite {
				// Make a copy before we stick it in the local block cache.
				block = block.DeepCopy()
				lbc[ptr] = block
			}
			return block, wasDirty, nil
		},
		func(ptr BlockPointer, block Block) error {
			lbc[ptr] = block.(*DirBlock)
			return nil
		}, fbo.log)
}

// newDirDataWithLBC is like `newDirDataWithLBCLocked`, but it must be
// called with `blockLock` unlocked, and the returned function must be
// called when the returned `dirData` is no longer in use.
func (fbo *folderBlockOps) newDirDataWithLBC(
	lState *lockState, dir path, chargedTo keybase1.UserOrTeamID,
	kmd KeyMetadata, lbc localBcache) (*dirData, func()) {
	// Lock and fetch for reading only, we want any dirty
	// blocks to go into the lbc.
	fbo.blockLock.RLock(lState)
	undoFn := func() { fbo.blockLock.RUnlock(lState) }
	return fbo.newDirDataWithLBCLocked(lState, dir, chargedTo, kmd, lbc), undoFn
}

func (fbo *folderBlockOps) makeDirDirtyLocked(
	lState *lockState, ptr BlockPointer, unrefs []BlockInfo) func() {
	fbo.blockLock.AssertLocked(lState)
	oldUnrefs, wasDirty := fbo.dirtyDirs[ptr]
	oldLen := len(oldUnrefs)
	fbo.dirtyDirs[ptr] = append(oldUnrefs, unrefs...)
	return func() {
		dirtyBcache := fbo.config.DirtyBlockCache()
		if wasDirty {
			fbo.dirtyDirs[ptr] = oldUnrefs[:oldLen:oldLen]
		} else {
			dirtyBcache.Delete(fbo.id(), ptr, fbo.branch())
			delete(fbo.dirtyDirs, ptr)
		}
		for _, unref := range unrefs {
			dirtyBcache.Delete(fbo.id(), unref.BlockPointer, fbo.branch())
		}
	}
}

func (fbo *folderBlockOps) updateParentDirEntryLocked(
	ctx context.Context, lState *lockState, dir path,
	kmd KeyMetadataWithRootDirEntry, setMtime, setCtime bool) (func(), error) {
	fbo.blockLock.AssertLocked(lState)
	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return nil, err
	}
	now := fbo.nowUnixNano()
	pp := *dir.parentPath()
	if pp.isValid() {
		dd := fbo.newDirDataLocked(lState, pp, chargedTo, kmd)
		de, err := dd.lookup(ctx, dir.tailName())
		if err != nil {
			return nil, err
		}
		newDe := de
		if setMtime {
			newDe.Mtime = now
		}
		if setCtime {
			newDe.Ctime = now
		}
		unrefs, err := dd.updateEntry(ctx, dir.tailName(), newDe)
		if err != nil {
			return nil, err
		}
		undoDirtyFn := fbo.makeDirDirtyLocked(lState, pp.tailPointer(), unrefs)
		return func() {
			_, _ = dd.updateEntry(ctx, dir.tailName(), de)
			undoDirtyFn()
		}, nil
	}

	// If the parent isn't a valid path, we need to update the root entry.
	var de *DirEntry
	if fbo.dirtyRootDirEntry == nil {
		deCopy := kmd.GetRootDirEntry()
		fbo.dirtyRootDirEntry = &deCopy
	} else {
		deCopy := *fbo.dirtyRootDirEntry
		de = &deCopy
	}
	if setMtime {
		fbo.dirtyRootDirEntry.Mtime = now
	}
	if setCtime {
		fbo.dirtyRootDirEntry.Ctime = now
	}
	return func() {
		fbo.dirtyRootDirEntry = de
	}, nil
}

func (fbo *folderBlockOps) addDirEntryInCacheLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	dir path, newName string, newDe DirEntry) (func(), error) {
	fbo.blockLock.AssertLocked(lState)

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return nil, err
	}
	dd := fbo.newDirDataLocked(lState, dir, chargedTo, kmd)
	unrefs, err := dd.addEntry(ctx, newName, newDe)
	if err != nil {
		return nil, err
	}
	parentUndo, err := fbo.updateParentDirEntryLocked(
		ctx, lState, dir, kmd, true, true)
	if err != nil {
		dd.removeEntry(ctx, newName)
		return nil, err
	}

	undoDirtyFn := fbo.makeDirDirtyLocked(lState, dir.tailPointer(), unrefs)
	return func() {
		_, _ = dd.removeEntry(ctx, newName)
		undoDirtyFn()
		parentUndo()
	}, nil
}

// AddDirEntryInCache adds a brand new entry to the given directory
// and updates the directory's own mtime and ctime.  It returns a
// function that can be called if the change needs to be undone.
func (fbo *folderBlockOps) AddDirEntryInCache(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	dir path, newName string, newDe DirEntry) (dirCacheUndoFn, error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	fn, err := fbo.addDirEntryInCacheLocked(
		ctx, lState, kmd, dir, newName, newDe)
	if err != nil {
		return nil, err
	}
	return fbo.wrapWithBlockLock(fn), nil
}

func (fbo *folderBlockOps) removeDirEntryInCacheLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	dir path, oldName string, oldDe DirEntry) (func(), error) {
	fbo.blockLock.AssertLocked(lState)

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return nil, err
	}
	dd := fbo.newDirDataLocked(lState, dir, chargedTo, kmd)
	unrefs, err := dd.removeEntry(ctx, oldName)
	if err != nil {
		return nil, err
	}
	if oldDe.Type == Dir {
		// The parent dir inherits any dirty unrefs from the removed
		// directory.
		if childUnrefs, ok := fbo.dirtyDirs[oldDe.BlockPointer]; ok {
			unrefs = append(unrefs, childUnrefs...)
		}
	}

	unlinkUndoFn := fbo.nodeCache.Unlink(
		oldDe.Ref(), dir.ChildPath(oldName, oldDe.BlockPointer), oldDe)

	parentUndo, err := fbo.updateParentDirEntryLocked(
		ctx, lState, dir, kmd, true, true)
	if err != nil {
		unlinkUndoFn()
		dd.addEntry(ctx, oldName, oldDe)
		return nil, err
	}

	undoDirtyFn := fbo.makeDirDirtyLocked(lState, dir.tailPointer(), unrefs)
	return func() {
		_, _ = dd.addEntry(ctx, oldName, oldDe)
		undoDirtyFn()
		parentUndo()
		unlinkUndoFn()
	}, nil
}

// RemoveDirEntryInCache removes an entry from the given directory //
// and updates the directory's own mtime and ctime.  It returns a
// function that can be called if the change needs to be undone.
func (fbo *folderBlockOps) RemoveDirEntryInCache(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	dir path, oldName string, oldDe DirEntry) (dirCacheUndoFn, error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	fn, err := fbo.removeDirEntryInCacheLocked(
		ctx, lState, kmd, dir, oldName, oldDe)
	if err != nil {
		return nil, err
	}
	return fbo.wrapWithBlockLock(fn), nil
}

// RenameDirEntryInCache updates the entries of both the old and new
// parent dirs for the given target dir atomically (with respect to
// blockLock).  It also updates the cache entry for the target, which
// would have its Ctime changed. The updates will get applied to the
// dirty blocks on subsequent fetches.
//
// The returned bool indicates whether or not the caller should clean
// up the target cache entry when the effects of the operation are no
// longer needed.
func (fbo *folderBlockOps) RenameDirEntryInCache(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	oldParent path, oldName string, newParent path, newName string,
	newDe DirEntry, replacedDe DirEntry) (undo dirCacheUndoFn, err error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	if newParent.tailPointer() == oldParent.tailPointer() &&
		oldName == newName {
		// Noop
		return nil, nil
	}

	var undoReplace func()
	if replacedDe.IsInitialized() {
		undoReplace, err = fbo.removeDirEntryInCacheLocked(
			ctx, lState, kmd, newParent, newName, replacedDe)
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if err != nil && undoReplace != nil {
			undoReplace()
		}
	}()

	undoAdd, err := fbo.addDirEntryInCacheLocked(
		ctx, lState, kmd, newParent, newName, newDe)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && undoAdd != nil {
			undoAdd()
		}
	}()

	undoRm, err := fbo.removeDirEntryInCacheLocked(
		ctx, lState, kmd, oldParent, oldName, DirEntry{})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && undoRm != nil {
			undoRm()
		}
	}()

	newParentNode := fbo.nodeCache.Get(newParent.tailRef())
	undoMove, err := fbo.nodeCache.Move(newDe.Ref(), newParentNode, newName)
	if err != nil {
		return nil, err
	}

	return fbo.wrapWithBlockLock(func() {
		if undoMove != nil {
			undoMove()
		}
		if undoRm != nil {
			undoRm()
		}
		if undoAdd != nil {
			undoAdd()
		}
		if undoReplace != nil {
			undoReplace()
		}
	}), nil
}

func (fbo *folderBlockOps) setCachedAttrLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	dir path, name string, attr attrChange, realEntry DirEntry) (
	dirCacheUndoFn, error) {
	fbo.blockLock.AssertLocked(lState)

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return nil, err
	}

	if !dir.isValid() {
		// Can't set attrs directly on the root entry, primarily
		// because there's no way to indicate it's dirty.  TODO: allow
		// mtime-setting on the root dir?
		return nil, InvalidParentPathError{dir}
	}
	var de DirEntry
	var unlinkedNode Node

	dd := fbo.newDirDataLocked(lState, dir, chargedTo, kmd)
	de, err = dd.lookup(ctx, name)
	if _, noExist := errors.Cause(err).(NoSuchNameError); noExist {
		// The node may be unlinked.
		unlinkedNode = fbo.nodeCache.Get(realEntry.Ref())
		if unlinkedNode != nil && !fbo.nodeCache.IsUnlinked(unlinkedNode) {
			unlinkedNode = nil
		}
		if unlinkedNode != nil {
			de = fbo.nodeCache.UnlinkedDirEntry(unlinkedNode)
		} else {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	oldDe := de
	switch attr {
	case exAttr:
		de.Type = realEntry.Type
	case mtimeAttr:
		de.Mtime = realEntry.Mtime
	}
	de.Ctime = realEntry.Ctime

	var undoDirtyFn func()
	if unlinkedNode != nil {
		fbo.nodeCache.UpdateUnlinkedDirEntry(unlinkedNode, de)
	} else {
		unrefs, err := dd.updateEntry(ctx, name, de)
		if err != nil {
			return nil, err
		}
		undoDirtyFn = fbo.makeDirDirtyLocked(lState, dir.tailPointer(), unrefs)
	}

	return fbo.wrapWithBlockLock(func() {
		if unlinkedNode != nil {
			fbo.nodeCache.UpdateUnlinkedDirEntry(unlinkedNode, oldDe)
		} else {
			_, _ = dd.updateEntry(ctx, name, oldDe)
			undoDirtyFn()
		}
	}), nil
}

// SetAttrInDirEntryInCache updates an entry from the given directory.
func (fbo *folderBlockOps) SetAttrInDirEntryInCache(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	p path, newDe DirEntry, attr attrChange) (dirCacheUndoFn, error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	return fbo.setCachedAttrLocked(
		ctx, lState, kmd, *p.parentPath(), p.tailName(), attr, newDe)
}

// getDirtyDirLocked composes getDirLocked and
// updateWithDirtyEntriesLocked. Note that a dirty dir means that it
// has entries possibly pointing to dirty files, and/or that its
// children list is dirty.
func (fbo *folderBlockOps) getDirtyDirLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadata, dir path, rtype blockReqType) (
	*DirBlock, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	dblock, _, err := fbo.getDirLocked(
		ctx, lState, kmd, dir.tailPointer(), dir, rtype)
	if err != nil {
		return nil, err
	}
	return dblock, err
}

// GetDirtyDir returns the directory block for a dirty directory,
// updated with all cached dirty entries.
func (fbo *folderBlockOps) GetDirtyDir(
	ctx context.Context, lState *lockState, kmd KeyMetadata, dir path,
	rtype blockReqType) (*DirBlock, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getDirtyDirLocked(ctx, lState, kmd, dir, rtype)
}

// GetChildren returns a map of EntryInfos for the (possibly dirty)
// children entries of the given directory.
func (fbo *folderBlockOps) GetChildren(
	ctx context.Context, lState *lockState, kmd KeyMetadata,
	dir path) (map[string]EntryInfo, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	dd := fbo.newDirDataLocked(lState, dir, keybase1.UserOrTeamID(""), kmd)
	return dd.getChildren(ctx)
}

// GetEntries returns a map of DirEntries for the (possibly dirty)
// children entries of the given directory.
func (fbo *folderBlockOps) GetEntries(
	ctx context.Context, lState *lockState, kmd KeyMetadata,
	dir path) (map[string]DirEntry, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	dd := fbo.newDirDataLocked(lState, dir, keybase1.UserOrTeamID(""), kmd)
	return dd.getEntries(ctx)
}

func (fbo *folderBlockOps) getEntryLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadataWithRootDirEntry, file path,
	includeDeleted bool) (de DirEntry, err error) {
	fbo.blockLock.AssertAnyLocked(lState)

	// See if this is the root.
	if !file.hasValidParent() {
		if fbo.dirtyRootDirEntry != nil {
			return *fbo.dirtyRootDirEntry, nil
		}
		return kmd.GetRootDirEntry(), nil
	}

	dd := fbo.newDirDataLocked(
		lState, *file.parentPath(), keybase1.UserOrTeamID(""), kmd)
	de, err = dd.lookup(ctx, file.tailName())
	_, noExist := errors.Cause(err).(NoSuchNameError)
	if includeDeleted && (noExist || de.BlockPointer != file.tailPointer()) {
		unlinkedNode := fbo.nodeCache.Get(file.tailPointer().Ref())
		if unlinkedNode != nil && fbo.nodeCache.IsUnlinked(unlinkedNode) {
			return fbo.nodeCache.UnlinkedDirEntry(unlinkedNode), nil
		}
		return DirEntry{}, err
	} else if err != nil {
		return DirEntry{}, err
	}
	return de, nil
}

// file must have a valid parent.
func (fbo *folderBlockOps) updateEntryLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadataWithRootDirEntry, file path,
	de DirEntry, includeDeleted bool) error {
	fbo.blockLock.AssertAnyLocked(lState)

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return err
	}
	parentPath := *file.parentPath()
	dd := fbo.newDirDataLocked(lState, parentPath, chargedTo, kmd)
	unrefs, err := dd.updateEntry(ctx, file.tailName(), de)
	_, noExist := errors.Cause(err).(NoSuchNameError)
	if noExist && includeDeleted {
		unlinkedNode := fbo.nodeCache.Get(file.tailPointer().Ref())
		if unlinkedNode != nil && fbo.nodeCache.IsUnlinked(unlinkedNode) {
			fbo.nodeCache.UpdateUnlinkedDirEntry(unlinkedNode, de)
			return nil
		}
		return err
	} else if err != nil {
		return err
	} else {
		_ = fbo.makeDirDirtyLocked(lState, parentPath.tailPointer(), unrefs)
	}
	return nil
}

// GetEntry returns the possibly-dirty DirEntry of the given file in
// its parent DirBlock. file must have a valid parent.
func (fbo *folderBlockOps) GetEntry(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	file path) (DirEntry, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getEntryLocked(ctx, lState, kmd, file, false)
}

// GetEntryEvenIfDeleted returns the possibly-dirty DirEntry of the
// given file in its parent DirBlock, even if the file has been
// deleted. file must have a valid parent.
func (fbo *folderBlockOps) GetEntryEvenIfDeleted(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	file path) (DirEntry, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.getEntryLocked(ctx, lState, kmd, file, true)
}

// Lookup returns the possibly-dirty DirEntry of the given file in its
// parent DirBlock, and a Node for the file if it exists.  It has to
// do all of this under the block lock to avoid races with
// UpdatePointers.
func (fbo *folderBlockOps) Lookup(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	dir Node, name string) (Node, DirEntry, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	dirPath := fbo.nodeCache.PathFromNode(dir)
	if !dirPath.isValid() {
		return nil, DirEntry{}, errors.WithStack(InvalidPathError{dirPath})
	}

	childPath := dirPath.ChildPathNoPtr(name)
	de, err := fbo.getEntryLocked(ctx, lState, kmd, childPath, false)
	if err != nil {
		return nil, DirEntry{}, err
	}

	if de.Type == Sym {
		return nil, de, nil
	}

	node, err := fbo.nodeCache.GetOrCreate(de.BlockPointer, name, dir)
	if err != nil {
		return nil, DirEntry{}, err
	}
	return node, de, nil
}

func (fbo *folderBlockOps) getOrCreateDirtyFileLocked(lState *lockState,
	file path) *dirtyFile {
	fbo.blockLock.AssertLocked(lState)
	ptr := file.tailPointer()
	df := fbo.dirtyFiles[ptr]
	if df == nil {
		df = newDirtyFile(file, fbo.config.DirtyBlockCache())
		fbo.dirtyFiles[ptr] = df
	}
	return df
}

// cacheBlockIfNotYetDirtyLocked puts a block into the cache, but only
// does so if the block isn't already marked as dirty in the cache.
// This is useful when operating on a dirty copy of a block that may
// already be in the cache.
func (fbo *folderBlockOps) cacheBlockIfNotYetDirtyLocked(
	lState *lockState, ptr BlockPointer, file path, block Block) error {
	fbo.blockLock.AssertLocked(lState)
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	needsCaching, isSyncing := df.setBlockDirty(ptr)

	if needsCaching {
		err := fbo.config.DirtyBlockCache().Put(fbo.id(), ptr, file.Branch,
			block)
		if err != nil {
			return err
		}
	}

	if isSyncing {
		fbo.doDeferWrite = true
	}
	return nil
}

func (fbo *folderBlockOps) getOrCreateSyncInfoLocked(
	lState *lockState, de DirEntry) (*syncInfo, error) {
	fbo.blockLock.AssertLocked(lState)
	ref := de.Ref()
	si, ok := fbo.unrefCache[ref]
	if !ok {
		so, err := newSyncOp(de.BlockPointer)
		if err != nil {
			return nil, err
		}
		si = &syncInfo{
			oldInfo: de.BlockInfo,
			op:      so,
		}
		fbo.unrefCache[ref] = si
	}
	return si, nil
}

// GetDirtyFileBlockRefs returns a list of references of all known dirty
// files.
func (fbo *folderBlockOps) GetDirtyFileBlockRefs(lState *lockState) []BlockRef {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	var dirtyRefs []BlockRef
	for ref := range fbo.unrefCache {
		dirtyRefs = append(dirtyRefs, ref)
	}
	return dirtyRefs
}

// GetDirtyDirBlockRefs returns a list of references of all known dirty
// directories.
func (fbo *folderBlockOps) GetDirtyDirBlockRefs(lState *lockState) []BlockRef {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	var dirtyRefs []BlockRef
	for ptr := range fbo.dirtyDirs {
		dirtyRefs = append(dirtyRefs, ptr.Ref())
	}
	return dirtyRefs
}

// getDirtyDirUnrefsLocked returns a list of block infos that need to be
// unreferenced for the given directory.
func (fbo *folderBlockOps) getDirtyDirUnrefsLocked(
	lState *lockState, ptr BlockPointer) []BlockInfo {
	fbo.blockLock.AssertRLocked(lState)
	return fbo.dirtyDirs[ptr]
}

// fixChildBlocksAfterRecoverableErrorLocked should be called when a sync
// failed with a recoverable block error on a multi-block file.  It
// makes sure that any outstanding dirty versions of the file are
// fixed up to reflect the fact that some of the indirect pointers now
// need to change.
func (fbo *folderBlockOps) fixChildBlocksAfterRecoverableErrorLocked(
	ctx context.Context, lState *lockState, file path, kmd KeyMetadata,
	redirtyOnRecoverableError map[BlockPointer]BlockPointer) {
	fbo.blockLock.AssertLocked(lState)

	defer func() {
		// Below, this function can end up writing dirty blocks back
		// to the cache, which will set `doDeferWrite` to `true`.
		// This leads to future writes being unnecessarily deferred
		// when a Sync is not happening, and can lead to dirty data
		// being synced twice and sticking around for longer than
		// needed.  So just reset `doDeferWrite` once we're
		// done. We're under `blockLock`, so this is safe.
		fbo.doDeferWrite = false
	}()

	df := fbo.dirtyFiles[file.tailPointer()]
	if df != nil {
		// Un-orphan old blocks, since we are reverting back to the
		// previous state.
		for _, oldPtr := range redirtyOnRecoverableError {
			fbo.log.CDebugf(ctx, "Un-orphaning %v", oldPtr)
			df.setBlockOrphaned(oldPtr, false)
		}
	}

	dirtyBcache := fbo.config.DirtyBlockCache()
	topBlock, err := dirtyBcache.Get(fbo.id(), file.tailPointer(), fbo.branch())
	fblock, ok := topBlock.(*FileBlock)
	if err != nil || !ok {
		fbo.log.CWarningf(ctx, "Couldn't find dirtied "+
			"top-block for %v: %v", file.tailPointer(), err)
		return
	}

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		fbo.log.CWarningf(ctx, "Couldn't find uid during recovery: %v", err)
		return
	}
	fd := fbo.newFileData(lState, file, chargedTo, kmd)

	// If a copy of the top indirect block was made, we need to
	// redirty all the sync'd blocks under their new IDs, so that
	// future syncs will know they failed.
	newPtrs := make(map[BlockPointer]bool, len(redirtyOnRecoverableError))
	for newPtr := range redirtyOnRecoverableError {
		newPtrs[newPtr] = true
	}
	found, err := fd.findIPtrsAndClearSize(ctx, fblock, newPtrs)
	if err != nil {
		fbo.log.CWarningf(
			ctx, "Couldn't find and clear iptrs during recovery: %v", err)
		return
	}
	for newPtr, oldPtr := range redirtyOnRecoverableError {
		if !found[newPtr] {
			continue
		}

		fbo.log.CDebugf(ctx, "Re-dirtying %v (and deleting dirty block %v)",
			newPtr, oldPtr)
		// These blocks would have been permanent, so they're
		// definitely still in the cache.
		b, err := fbo.config.BlockCache().Get(newPtr)
		if err != nil {
			fbo.log.CWarningf(ctx, "Couldn't re-dirty %v: %v", newPtr, err)
			continue
		}
		if err = fbo.cacheBlockIfNotYetDirtyLocked(
			lState, newPtr, file, b); err != nil {
			fbo.log.CWarningf(ctx, "Couldn't re-dirty %v: %v", newPtr, err)
		}
		fbo.log.CDebugf(ctx, "Deleting dirty ptr %v after recoverable error",
			oldPtr)
		err = dirtyBcache.Delete(fbo.id(), oldPtr, fbo.branch())
		if err != nil {
			fbo.log.CDebugf(ctx, "Couldn't del-dirty %v: %v", oldPtr, err)
		}
	}
}

func (fbo *folderBlockOps) nowUnixNano() int64 {
	return fbo.config.Clock().Now().UnixNano()
}

// PrepRename prepares the given rename operation. It returns the old
// and new parent block (which may be the same, and which shouldn't be
// modified), and what is to be the new DirEntry.
func (fbo *folderBlockOps) PrepRename(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	oldParent path, oldName string, newParent path, newName string) (
	newDe, replacedDe DirEntry, ro *renameOp,
	err error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	// Look up in the old path. Won't be modified, so only fetch for reading.
	newDe, err = fbo.getEntryLocked(
		ctx, lState, kmd, oldParent.ChildPathNoPtr(oldName), false)
	if err != nil {
		return DirEntry{}, DirEntry{}, nil, err
	}

	oldParentPtr := oldParent.tailPointer()
	newParentPtr := newParent.tailPointer()
	ro, err = newRenameOp(oldName, oldParentPtr, newName, newParentPtr,
		newDe.BlockPointer, newDe.Type)
	if err != nil {
		return DirEntry{}, DirEntry{}, nil, err
	}
	ro.AddUpdate(oldParentPtr, oldParentPtr)
	ro.setFinalPath(newParent)
	ro.oldFinalPath = oldParent
	if oldParentPtr.ID != newParentPtr.ID {
		ro.AddUpdate(newParentPtr, newParentPtr)
	}

	replacedDe, err = fbo.getEntryLocked(
		ctx, lState, kmd, newParent.ChildPathNoPtr(newName), false)
	if _, notExists := errors.Cause(err).(NoSuchNameError); notExists {
		return newDe, DirEntry{}, ro, nil
	} else if err != nil {
		return DirEntry{}, DirEntry{}, nil, err
	}

	return newDe, replacedDe, ro, nil
}

func (fbo *folderBlockOps) newFileData(lState *lockState,
	file path, chargedTo keybase1.UserOrTeamID, kmd KeyMetadata) *fileData {
	fbo.blockLock.AssertAnyLocked(lState)
	return newFileData(file, chargedTo, fbo.config.Crypto(),
		fbo.config.BlockSplitter(), kmd,
		func(ctx context.Context, kmd KeyMetadata, ptr BlockPointer,
			file path, rtype blockReqType) (*FileBlock, bool, error) {
			lState := lState
			if rtype == blockReadParallel {
				lState = nil
			}
			return fbo.getFileBlockLocked(
				ctx, lState, kmd, ptr, file, rtype)
		},
		func(ptr BlockPointer, block Block) error {
			return fbo.cacheBlockIfNotYetDirtyLocked(
				lState, ptr, file, block)
		}, fbo.log)
}

func (fbo *folderBlockOps) newFileDataWithCache(lState *lockState,
	file path, chargedTo keybase1.UserOrTeamID, kmd KeyMetadata,
	dirtyBcache DirtyBlockCache) *fileData {
	fbo.blockLock.AssertAnyLocked(lState)
	return newFileData(file, chargedTo, fbo.config.Crypto(),
		fbo.config.BlockSplitter(), kmd,
		func(ctx context.Context, kmd KeyMetadata, ptr BlockPointer,
			file path, rtype blockReqType) (*FileBlock, bool, error) {
			block, err := dirtyBcache.Get(file.Tlf, ptr, file.Branch)
			if fblock, ok := block.(*FileBlock); ok && err == nil {
				return fblock, true, nil
			}
			lState := lState
			if rtype == blockReadParallel {
				lState = nil
			}
			return fbo.getFileBlockLocked(
				ctx, lState, kmd, ptr, file, rtype)
		},
		func(ptr BlockPointer, block Block) error {
			return dirtyBcache.Put(file.Tlf, ptr, file.Branch, block)
		}, fbo.log)
}

// Read reads from the given file into the given buffer at the given
// offset. It returns the number of bytes read and nil, or 0 and the
// error if there was one.
func (fbo *folderBlockOps) Read(
	ctx context.Context, lState *lockState, kmd KeyMetadata, file Node,
	dest []byte, off int64) (int64, error) {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	filePath := fbo.nodeCache.PathFromNode(file)

	fbo.log.CDebugf(ctx, "Reading from %v", filePath.tailPointer())

	var id keybase1.UserOrTeamID // Data reads don't depend on the id.
	fd := fbo.newFileData(lState, filePath, id, kmd)
	return fd.read(ctx, dest, Int64Offset(off))
}

func (fbo *folderBlockOps) maybeWaitOnDeferredWrites(
	ctx context.Context, lState *lockState, file Node,
	c DirtyPermChan) error {
	var errListener chan error
	registerErr := func() error {
		fbo.blockLock.Lock(lState)
		defer fbo.blockLock.Unlock(lState)
		filePath, err := fbo.pathFromNodeForBlockWriteLocked(lState, file)
		if err != nil {
			return err
		}
		df := fbo.getOrCreateDirtyFileLocked(lState, filePath)
		errListener = make(chan error, 1)
		df.addErrListener(errListener)
		return nil
	}
	err := registerErr()
	if err != nil {
		return err
	}

	logTimer := time.After(100 * time.Millisecond)
	doLogUnblocked := false
	for {
		var err error
		select {
		case <-c:
			if doLogUnblocked {
				fbo.log.CDebugf(ctx, "Write unblocked")
			}
			// Make sure there aren't any queued errors.
			select {
			case err = <-errListener:
				// Break the select to check the cause of the error below.
				break
			default:
			}
			return nil
		case <-logTimer:
			// Print a log message once if it's taking too long.
			fbo.log.CDebugf(ctx,
				"Blocking a write because of a full dirty buffer")
			doLogUnblocked = true
		case <-ctx.Done():
			return ctx.Err()
		case err = <-errListener:
			// Fall through to check the cause of the error below.
		}
		// Context errors are safe to ignore, since they are likely to
		// be specific to a previous sync (e.g., a user hit ctrl-c
		// during an fsync, or a sync timed out, or a test was
		// provoking an error specifically [KBFS-2164]).
		cause := errors.Cause(err)
		if cause == context.Canceled || cause == context.DeadlineExceeded {
			fbo.log.CDebugf(ctx, "Ignoring sync err: %+v", err)
			err := registerErr()
			if err != nil {
				return err
			}
			continue
		} else if err != nil {
			// Treat other errors as fatal to this write -- e.g., the
			// user's quota is full, the local journal is broken,
			// etc. XXX: should we ignore errors that are specific
			// only to some other file being sync'd (e.g.,
			// "recoverable" block errors from which we couldn't
			// recover)?
			return err
		}
	}
}

func (fbo *folderBlockOps) pathFromNodeForBlockWriteLocked(
	lState *lockState, n Node) (path, error) {
	fbo.blockLock.AssertLocked(lState)
	p := fbo.nodeCache.PathFromNode(n)
	if !p.isValid() {
		return path{}, errors.WithStack(InvalidPathError{p})
	}
	return p, nil
}

// writeGetFileLocked checks write permissions explicitly for
// writeDataLocked, truncateLocked etc and returns
func (fbo *folderBlockOps) writeGetFileLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadata,
	file path) (*FileBlock, error) {
	fbo.blockLock.AssertLocked(lState)

	session, err := fbo.config.KBPKI().GetCurrentSession(ctx)
	if err != nil {
		return nil, err
	}
	isWriter, err := kmd.IsWriter(
		ctx, fbo.config.KBPKI(), session.UID, session.VerifyingKey)
	if err != nil {
		return nil, err
	}
	if !isWriter {
		return nil, NewWriteAccessError(kmd.GetTlfHandle(),
			session.Name, file.String())
	}
	fblock, err := fbo.getFileLocked(ctx, lState, kmd, file, blockWrite)
	if err != nil {
		return nil, err
	}
	return fblock, nil
}

// Returns the set of blocks dirtied during this write that might need
// to be cleaned up if the write is deferred.
func (fbo *folderBlockOps) writeDataLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	file path, data []byte, off int64) (
	latestWrite WriteRange, dirtyPtrs []BlockPointer,
	newlyDirtiedChildBytes int64, err error) {
	if jServer, err := GetJournalServer(fbo.config); err == nil {
		jServer.dirtyOpStart(fbo.id())
		defer jServer.dirtyOpEnd(fbo.id())
	}

	fbo.blockLock.AssertLocked(lState)
	fbo.log.CDebugf(ctx, "writeDataLocked on file pointer %v",
		file.tailPointer())
	defer func() {
		fbo.log.CDebugf(ctx, "writeDataLocked done: %v", err)
	}()

	fblock, err := fbo.writeGetFileLocked(ctx, lState, kmd, file)
	if err != nil {
		return WriteRange{}, nil, 0, err
	}

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return WriteRange{}, nil, 0, err
	}

	fd := fbo.newFileData(lState, file, chargedTo, kmd)

	dirtyBcache := fbo.config.DirtyBlockCache()
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	defer func() {
		// Always update unsynced bytes and potentially force a sync,
		// even on an error, since the previously-dirty bytes stay in
		// the cache.
		df.updateNotYetSyncingBytes(newlyDirtiedChildBytes)
		if dirtyBcache.ShouldForceSync(fbo.id()) {
			select {
			// If we can't send on the channel, that means a sync is
			// already in progress.
			case fbo.forceSyncChan <- struct{}{}:
				fbo.log.CDebugf(ctx, "Forcing a sync due to full buffer")
			default:
			}
		}
	}()

	de, err := fbo.getEntryLocked(ctx, lState, kmd, file, true)
	if err != nil {
		return WriteRange{}, nil, 0, err
	}
	if de.BlockPointer != file.tailPointer() {
		fbo.log.CDebugf(ctx, "DirEntry and file tail pointer don't match: "+
			"%v vs %v", de.BlockPointer, file.tailPointer())
	}

	si, err := fbo.getOrCreateSyncInfoLocked(lState, de)
	if err != nil {
		return WriteRange{}, nil, 0, err
	}

	newDe, dirtyPtrs, unrefs, newlyDirtiedChildBytes, bytesExtended, err :=
		fd.write(ctx, data, Int64Offset(off), fblock, de, df)
	// Record the unrefs before checking the error so we remember the
	// state of newly dirtied blocks.
	si.unrefs = append(si.unrefs, unrefs...)
	if err != nil {
		return WriteRange{}, nil, newlyDirtiedChildBytes, err
	}

	// Update the file's directory entry.
	now := fbo.nowUnixNano()
	newDe.Mtime = now
	newDe.Ctime = now
	err = fbo.updateEntryLocked(ctx, lState, kmd, file, newDe, true)
	if err != nil {
		return WriteRange{}, nil, newlyDirtiedChildBytes, err
	}

	if fbo.doDeferWrite {
		df.addDeferredNewBytes(bytesExtended)
	}

	latestWrite = si.op.addWrite(uint64(off), uint64(len(data)))

	return latestWrite, dirtyPtrs, newlyDirtiedChildBytes, nil
}

// Write writes the given data to the given file. May block if there
// is too much unflushed data; in that case, it will be unblocked by a
// future sync.
func (fbo *folderBlockOps) Write(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	file Node, data []byte, off int64) error {
	// If there is too much unflushed data, we should wait until some
	// of it gets flush so our memory usage doesn't grow without
	// bound.
	c, err := fbo.config.DirtyBlockCache().RequestPermissionToDirty(ctx,
		fbo.id(), int64(len(data)))
	if err != nil {
		return err
	}
	defer fbo.config.DirtyBlockCache().UpdateUnsyncedBytes(fbo.id(),
		-int64(len(data)), false)
	err = fbo.maybeWaitOnDeferredWrites(ctx, lState, file, c)
	if err != nil {
		return err
	}

	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	filePath, err := fbo.pathFromNodeForBlockWriteLocked(lState, file)
	if err != nil {
		return err
	}

	defer func() {
		fbo.doDeferWrite = false
	}()

	latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err := fbo.writeDataLocked(
		ctx, lState, kmd, filePath, data, off)
	if err != nil {
		return err
	}

	fbo.observers.localChange(ctx, file, latestWrite)

	if fbo.doDeferWrite {
		// There's an ongoing sync, and this write altered dirty
		// blocks that are in the process of syncing.  So, we have to
		// redo this write once the sync is complete, using the new
		// file path.
		//
		// There is probably a less terrible of doing this that
		// doesn't involve so much copying and rewriting, but this is
		// the most obviously correct way.
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		fbo.log.CDebugf(ctx, "Deferring a write to file %v off=%d len=%d",
			filePath.tailPointer(), off, len(data))
		ds := fbo.deferred[filePath.tailRef()]
		ds.dirtyDeletes = append(ds.dirtyDeletes, dirtyPtrs...)
		ds.writes = append(ds.writes,
			func(ctx context.Context, lState *lockState,
				kmd KeyMetadataWithRootDirEntry, f path) error {
				// We are about to re-dirty these bytes, so mark that
				// they will no longer be synced via the old file.
				df := fbo.getOrCreateDirtyFileLocked(lState, filePath)
				df.updateNotYetSyncingBytes(-newlyDirtiedChildBytes)

				// Write the data again.  We know this won't be
				// deferred, so no need to check the new ptrs.
				_, _, _, err = fbo.writeDataLocked(
					ctx, lState, kmd, f, dataCopy, off)
				return err
			})
		ds.waitBytes += newlyDirtiedChildBytes
		fbo.deferred[filePath.tailRef()] = ds
	}

	return nil
}

// truncateExtendLocked is called by truncateLocked to extend a file and
// creates a hole.
func (fbo *folderBlockOps) truncateExtendLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	file path, size uint64, parentBlocks []parentBlockAndChildIndex) (
	WriteRange, []BlockPointer, error) {
	fblock, err := fbo.writeGetFileLocked(ctx, lState, kmd, file)
	if err != nil {
		return WriteRange{}, nil, err
	}

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return WriteRange{}, nil, err
	}

	fd := fbo.newFileData(lState, file, chargedTo, kmd)

	de, err := fbo.getEntryLocked(ctx, lState, kmd, file, true)
	if err != nil {
		return WriteRange{}, nil, err
	}
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	newDe, dirtyPtrs, err := fd.truncateExtend(
		ctx, size, fblock, parentBlocks, de, df)
	if err != nil {
		return WriteRange{}, nil, err
	}

	now := fbo.nowUnixNano()
	newDe.Mtime = now
	newDe.Ctime = now
	err = fbo.updateEntryLocked(ctx, lState, kmd, file, newDe, true)
	if err != nil {
		return WriteRange{}, nil, err
	}

	si, err := fbo.getOrCreateSyncInfoLocked(lState, de)
	if err != nil {
		return WriteRange{}, nil, err
	}
	latestWrite := si.op.addTruncate(size)

	if fbo.config.DirtyBlockCache().ShouldForceSync(fbo.id()) {
		select {
		// If we can't send on the channel, that means a sync is
		// already in progress
		case fbo.forceSyncChan <- struct{}{}:
			fbo.log.CDebugf(ctx, "Forcing a sync due to full buffer")
		default:
		}
	}

	fbo.log.CDebugf(ctx, "truncateExtendLocked: done")
	return latestWrite, dirtyPtrs, nil
}

// Returns the set of newly-ID'd blocks created during this truncate
// that might need to be cleaned up if the truncate is deferred.
func (fbo *folderBlockOps) truncateLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	file path, size uint64) (*WriteRange, []BlockPointer, int64, error) {
	if jServer, err := GetJournalServer(fbo.config); err == nil {
		jServer.dirtyOpStart(fbo.id())
		defer jServer.dirtyOpEnd(fbo.id())
	}

	fblock, err := fbo.writeGetFileLocked(ctx, lState, kmd, file)
	if err != nil {
		return &WriteRange{}, nil, 0, err
	}

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return &WriteRange{}, nil, 0, err
	}

	fd := fbo.newFileData(lState, file, chargedTo, kmd)

	// find the block where the file should now end
	iSize := int64(size) // TODO: deal with overflow
	_, parentBlocks, block, nextBlockOff, startOff, _, err :=
		fd.getFileBlockAtOffset(ctx, fblock, Int64Offset(iSize), blockWrite)
	if err != nil {
		return &WriteRange{}, nil, 0, err
	}

	currLen := int64(startOff) + int64(len(block.Contents))
	if currLen+truncateExtendCutoffPoint < iSize {
		latestWrite, dirtyPtrs, err := fbo.truncateExtendLocked(
			ctx, lState, kmd, file, uint64(iSize), parentBlocks)
		if err != nil {
			return &latestWrite, dirtyPtrs, 0, err
		}
		return &latestWrite, dirtyPtrs, 0, err
	} else if currLen < iSize {
		moreNeeded := iSize - currLen
		latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err :=
			fbo.writeDataLocked(ctx, lState, kmd, file,
				make([]byte, moreNeeded, moreNeeded), currLen)
		if err != nil {
			return &latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err
		}
		return &latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err
	} else if currLen == iSize && nextBlockOff < 0 {
		// same size!
		return nil, nil, 0, nil
	}

	// update the local entry size
	de, err := fbo.getEntryLocked(ctx, lState, kmd, file, true)
	if err != nil {
		return nil, nil, 0, err
	}

	si, err := fbo.getOrCreateSyncInfoLocked(lState, de)
	if err != nil {
		return nil, nil, 0, err
	}

	newDe, dirtyPtrs, unrefs, newlyDirtiedChildBytes, err := fd.truncateShrink(
		ctx, size, fblock, de)
	// Record the unrefs before checking the error so we remember the
	// state of newly dirtied blocks.
	si.unrefs = append(si.unrefs, unrefs...)
	if err != nil {
		return nil, nil, newlyDirtiedChildBytes, err
	}

	// Update dirtied bytes and unrefs regardless of error.
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	df.updateNotYetSyncingBytes(newlyDirtiedChildBytes)

	latestWrite := si.op.addTruncate(size)
	now := fbo.nowUnixNano()
	newDe.Mtime = now
	newDe.Ctime = now
	err = fbo.updateEntryLocked(ctx, lState, kmd, file, newDe, true)
	if err != nil {
		return nil, nil, newlyDirtiedChildBytes, err
	}

	return &latestWrite, dirtyPtrs, newlyDirtiedChildBytes, nil
}

// Truncate truncates or extends the given file to the given size.
// May block if there is too much unflushed data; in that case, it
// will be unblocked by a future sync.
func (fbo *folderBlockOps) Truncate(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	file Node, size uint64) error {
	// If there is too much unflushed data, we should wait until some
	// of it gets flush so our memory usage doesn't grow without
	// bound.
	//
	// Assume the whole remaining file will be dirty after this
	// truncate.  TODO: try to figure out how many bytes actually will
	// be dirtied ahead of time?
	c, err := fbo.config.DirtyBlockCache().RequestPermissionToDirty(ctx,
		fbo.id(), int64(size))
	if err != nil {
		return err
	}
	defer fbo.config.DirtyBlockCache().UpdateUnsyncedBytes(fbo.id(),
		-int64(size), false)
	err = fbo.maybeWaitOnDeferredWrites(ctx, lState, file, c)
	if err != nil {
		return err
	}

	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	filePath, err := fbo.pathFromNodeForBlockWriteLocked(lState, file)
	if err != nil {
		return err
	}

	defer func() {
		fbo.doDeferWrite = false
	}()

	latestWrite, dirtyPtrs, newlyDirtiedChildBytes, err := fbo.truncateLocked(
		ctx, lState, kmd, filePath, size)
	if err != nil {
		return err
	}

	if latestWrite != nil {
		fbo.observers.localChange(ctx, file, *latestWrite)
	}

	if fbo.doDeferWrite {
		// There's an ongoing sync, and this truncate altered
		// dirty blocks that are in the process of syncing.  So,
		// we have to redo this truncate once the sync is complete,
		// using the new file path.
		fbo.log.CDebugf(ctx, "Deferring a truncate to file %v",
			filePath.tailPointer())
		ds := fbo.deferred[filePath.tailRef()]
		ds.dirtyDeletes = append(ds.dirtyDeletes, dirtyPtrs...)
		ds.writes = append(ds.writes,
			func(ctx context.Context, lState *lockState,
				kmd KeyMetadataWithRootDirEntry, f path) error {
				// We are about to re-dirty these bytes, so mark that
				// they will no longer be synced via the old file.
				df := fbo.getOrCreateDirtyFileLocked(lState, filePath)
				df.updateNotYetSyncingBytes(-newlyDirtiedChildBytes)

				// Truncate the file again.  We know this won't be
				// deferred, so no need to check the new ptrs.
				_, _, _, err := fbo.truncateLocked(
					ctx, lState, kmd, f, size)
				return err
			})
		ds.waitBytes += newlyDirtiedChildBytes
		fbo.deferred[filePath.tailRef()] = ds
	}

	return nil
}

// IsDirty returns whether the given file is dirty; if false is
// returned, then the file doesn't need to be synced.
func (fbo *folderBlockOps) IsDirty(lState *lockState, file path) bool {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	// A dirty file should probably match all three of these, but
	// check them individually just in case.
	if fbo.config.DirtyBlockCache().IsDirty(
		fbo.id(), file.tailPointer(), file.Branch) {
		return true
	}

	if _, ok := fbo.dirtyFiles[file.tailPointer()]; ok {
		return ok
	}

	_, ok := fbo.unrefCache[file.tailRef()]
	return ok
}

func (fbo *folderBlockOps) clearCacheInfoLocked(lState *lockState,
	file path) error {
	fbo.blockLock.AssertLocked(lState)
	ref := file.tailRef()
	delete(fbo.unrefCache, ref)
	df := fbo.dirtyFiles[file.tailPointer()]
	if df != nil {
		err := df.finishSync()
		if err != nil {
			return err
		}
		delete(fbo.dirtyFiles, file.tailPointer())
	}
	return nil
}

func (fbo *folderBlockOps) clearAllDirtyDirsLocked(
	ctx context.Context, lState *lockState, kmd KeyMetadata) {
	fbo.blockLock.AssertLocked(lState)
	dirtyBCache := fbo.config.DirtyBlockCache()
	for ptr := range fbo.dirtyDirs {
		dir := path{
			FolderBranch: fbo.folderBranch,
			path:         []pathNode{{ptr, ptr.String()}},
		}
		dd := fbo.newDirDataLocked(lState, dir, keybase1.UserOrTeamID(""), kmd)
		childPtrs, err := dd.getDirtyChildPtrs(ctx, dirtyBCache)
		if err != nil {
			fbo.log.CDebugf(ctx, "Failed to get child ptrs for %v: %+v",
				ptr, err)
		}
		for childPtr := range childPtrs {
			err := dirtyBCache.Delete(fbo.id(), childPtr, fbo.branch())
			if err != nil {
				fbo.log.CDebugf(
					ctx, "Failed to delete %v from dirty "+"cache: %+v",
					childPtr, err)
			}
		}

		err = dirtyBCache.Delete(fbo.id(), ptr, fbo.branch())
		if err != nil {
			fbo.log.CDebugf(ctx, "Failed to delete %v from dirty cache: %+v",
				ptr, err)
		}
	}
	fbo.dirtyDirs = make(map[BlockPointer][]BlockInfo)
	fbo.dirtyRootDirEntry = nil
}

// ClearCacheInfo removes any cached info for the the given file.
func (fbo *folderBlockOps) ClearCacheInfo(lState *lockState, file path) error {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	return fbo.clearCacheInfoLocked(lState, file)
}

// revertSyncInfoAfterRecoverableError updates the saved sync info to
// include all the blocks from before the error, except for those that
// have encountered recoverable block errors themselves.
func (fbo *folderBlockOps) revertSyncInfoAfterRecoverableError(
	blocksToRemove []BlockPointer, result fileSyncState) {
	si := result.si
	savedSi := result.savedSi

	// Save the blocks we need to clean up on the next attempt.
	toClean := si.toCleanIfUnused

	newIndirect := make(map[BlockPointer]bool)
	for _, ptr := range result.newIndirectFileBlockPtrs {
		newIndirect[ptr] = true
	}

	// Propagate all unrefs forward, except those that belong to new
	// blocks that were created during the sync.
	unrefs := make([]BlockInfo, 0, len(si.unrefs))
	for _, unref := range si.unrefs {
		if newIndirect[unref.BlockPointer] {
			fbo.log.CDebugf(nil, "Dropping unref %v", unref)
			continue
		}
		unrefs = append(unrefs, unref)
	}

	// This sync will be retried and needs new blocks, so
	// reset everything in the sync info.
	*si = *savedSi
	si.toCleanIfUnused = toClean
	si.unrefs = unrefs
	if si.bps == nil {
		return
	}

	si.bps.blockStates = nil

	// Mark any bad pointers so they get skipped next time.
	blocksToRemoveSet := make(map[BlockPointer]bool)
	for _, ptr := range blocksToRemove {
		blocksToRemoveSet[ptr] = true
	}

	for _, bs := range savedSi.bps.blockStates {
		// Only save the good pointers
		if !blocksToRemoveSet[bs.blockPtr] {
			si.bps.blockStates = append(si.bps.blockStates, bs)
		}
	}
}

// ReadyBlock is a thin wrapper around BlockOps.Ready() that handles
// checking for duplicates.
func ReadyBlock(ctx context.Context, bcache BlockCache, bops BlockOps,
	crypto cryptoPure, kmd KeyMetadata, block Block,
	chargedTo keybase1.UserOrTeamID, bType keybase1.BlockType) (
	info BlockInfo, plainSize int, readyBlockData ReadyBlockData, err error) {
	var ptr BlockPointer
	directType := DirectBlock
	if block.IsIndirect() {
		directType = IndirectBlock
	} else if fBlock, ok := block.(*FileBlock); ok {
		// first see if we are duplicating any known blocks in this folder
		ptr, err = bcache.CheckForKnownPtr(kmd.TlfID(), fBlock)
		if err != nil {
			return
		}
	}

	// Ready the block, even in the case where we can reuse an
	// existing block, just so that we know what the size of the
	// encrypted data will be.
	bid, plainSize, readyBlockData, err := bops.Ready(ctx, kmd, block)
	if err != nil {
		return
	}

	if ptr.IsInitialized() {
		ptr.RefNonce, err = crypto.MakeBlockRefNonce()
		if err != nil {
			return
		}
		ptr.SetWriter(chargedTo)
		// In case we're deduping an old pointer with an unknown block type.
		ptr.DirectType = directType
	} else {
		ptr = BlockPointer{
			ID:         bid,
			KeyGen:     kmd.LatestKeyGeneration(),
			DataVer:    block.DataVersion(),
			DirectType: directType,
			Context:    kbfsblock.MakeFirstContext(chargedTo, bType),
		}
	}

	info = BlockInfo{
		BlockPointer: ptr,
		EncodedSize:  uint32(readyBlockData.GetEncodedSize()),
	}
	return
}

// fileSyncState holds state for a sync operation for a single
// file.
type fileSyncState struct {
	// If fblock is non-nil, the (dirty, indirect, cached) block
	// it points to will be set to savedFblock on a recoverable
	// error.
	fblock, savedFblock *FileBlock

	// redirtyOnRecoverableError, which is non-nil only when fblock is
	// non-nil, contains pointers that need to be re-dirtied if the
	// top block gets copied during the sync, and a recoverable error
	// happens.  Maps to the old block pointer for the block, which
	// would need a DirtyBlockCache.Delete.
	redirtyOnRecoverableError map[BlockPointer]BlockPointer

	// If si is non-nil, its updated state will be reset on
	// error. Also, if the error is recoverable, it will be
	// reverted to savedSi.
	//
	// TODO: Working with si in this way is racy, since si is a
	// member of unrefCache.
	si, savedSi *syncInfo

	// oldFileBlockPtrs is a list of transient entries in the
	// block cache for the file, which should be removed when the
	// sync finishes.
	oldFileBlockPtrs []BlockPointer

	// newIndirectFileBlockPtrs is a list of permanent entries
	// added to the block cache for the file, which should be
	// removed after the blocks have been sent to the server.
	// They are not removed on an error, because in that case the
	// file is still dirty locally and may get another chance to
	// be sync'd.
	//
	// TODO: This can be a list of IDs instead.
	newIndirectFileBlockPtrs []BlockPointer
}

// startSyncWrite contains the portion of StartSync() that's done
// while write-locking blockLock.  If there is no dirty de cache
// entry, dirtyDe will be nil.
func (fbo *folderBlockOps) startSyncWrite(ctx context.Context,
	lState *lockState, md *RootMetadata, file path) (
	fblock *FileBlock, bps *blockPutState, syncState fileSyncState,
	dirtyDe *DirEntry, err error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	// update the parent directories, and write all the new blocks out
	// to disk
	fblock, err = fbo.getFileLocked(ctx, lState, md.ReadOnly(), file, blockWrite)
	if err != nil {
		return nil, nil, syncState, nil, err
	}

	fileRef := file.tailRef()
	si, ok := fbo.unrefCache[fileRef]
	if !ok {
		return nil, nil, syncState, nil,
			fmt.Errorf("No syncOp found for file ref %v", fileRef)
	}

	// Collapse the write range to reduce the size of the sync op.
	si.op.Writes = si.op.collapseWriteRange(nil)
	// If this function returns a success, we need to make sure the op
	// in `md` is not the same variable as the op in `unrefCache`,
	// because the latter could get updated still by local writes
	// before `md` is flushed to the server.  We don't copy it here
	// because code below still needs to modify it (and by extension,
	// the one stored in `syncState.si`).
	si.op.setFinalPath(file)
	md.AddOp(si.op)

	// Fill in syncState.
	if fblock.IsInd {
		fblockCopy := fblock.DeepCopy()
		syncState.fblock = fblock
		syncState.savedFblock = fblockCopy
		syncState.redirtyOnRecoverableError = make(map[BlockPointer]BlockPointer)
	}
	syncState.si = si
	syncState.savedSi, err = si.DeepCopy(fbo.config.Codec())
	if err != nil {
		return nil, nil, syncState, nil, err
	}

	if si.bps == nil {
		si.bps = newBlockPutState(1)
	} else {
		// reinstate byte accounting from the previous Sync
		md.SetRefBytes(si.refBytes)
		md.AddDiskUsage(si.refBytes)
		md.SetUnrefBytes(si.unrefBytes)
		md.SetMDRefBytes(0) // this will be calculated anew
		md.SetDiskUsage(md.DiskUsage() - si.unrefBytes)
		syncState.newIndirectFileBlockPtrs = append(
			syncState.newIndirectFileBlockPtrs, si.op.Refs()...)
	}
	defer func() {
		si.refBytes = md.RefBytes()
		si.unrefBytes = md.UnrefBytes()
	}()

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, md)
	if err != nil {
		return nil, nil, syncState, nil, err
	}

	dirtyBcache := fbo.config.DirtyBlockCache()
	df := fbo.getOrCreateDirtyFileLocked(lState, file)
	fd := fbo.newFileData(lState, file, chargedTo, md.ReadOnly())

	// Note: below we add possibly updated file blocks as "unref" and
	// "ref" blocks.  This is fine, since conflict resolution or
	// notifications will never happen within a file.

	// If needed, split the children blocks up along new boundaries
	// (e.g., if using a fingerprint-based block splitter).
	unrefs, err := fd.split(ctx, fbo.id(), dirtyBcache, fblock, df)
	// Preserve any unrefs before checking the error.
	for _, unref := range unrefs {
		md.AddUnrefBlock(unref)
	}
	if err != nil {
		return nil, nil, syncState, nil, err
	}

	// Ready all children blocks, if any.
	oldPtrs, err := fd.ready(ctx, fbo.id(), fbo.config.BlockCache(),
		fbo.config.DirtyBlockCache(), fbo.config.BlockOps(), si.bps, fblock, df)
	if err != nil {
		return nil, nil, syncState, nil, err
	}

	for newInfo, oldPtr := range oldPtrs {
		syncState.newIndirectFileBlockPtrs = append(
			syncState.newIndirectFileBlockPtrs, newInfo.BlockPointer)
		df.setBlockOrphaned(oldPtr, true)

		// Defer the DirtyBlockCache.Delete until after the new path
		// is ready, in case anyone tries to read the dirty file in
		// the meantime.
		syncState.oldFileBlockPtrs = append(syncState.oldFileBlockPtrs, oldPtr)

		md.AddRefBlock(newInfo)

		// If this block is replacing a block from a previous, failed
		// Sync, we need to take that block out of the refs list, and
		// avoid unrefing it as well.
		si.removeReplacedBlock(ctx, fbo.log, oldPtr)

		err = df.setBlockSyncing(oldPtr)
		if err != nil {
			return nil, nil, syncState, nil, err
		}
		syncState.redirtyOnRecoverableError[newInfo.BlockPointer] = oldPtr
	}

	err = df.setBlockSyncing(file.tailPointer())
	if err != nil {
		return nil, nil, syncState, nil, err
	}
	syncState.oldFileBlockPtrs = append(
		syncState.oldFileBlockPtrs, file.tailPointer())

	// Capture the current de before we release the block lock, so
	// other deferred writes don't slip in.
	dd := fbo.newDirDataLocked(lState, *file.parentPath(), chargedTo, md)
	de, err := dd.lookup(ctx, file.tailName())
	if err != nil {
		return nil, nil, syncState, nil, err
	}
	dirtyDe = &de

	// Leave a copy of the syncOp in `unrefCache`, since it may be
	// modified by future local writes while the syncOp in `md` should
	// only be modified by the rest of this sync process.
	var syncOpCopy *syncOp
	err = kbfscodec.Update(fbo.config.Codec(), &syncOpCopy, si.op)
	if err != nil {
		return nil, nil, syncState, nil, err
	}
	fbo.unrefCache[fileRef].op = syncOpCopy

	// If there are any deferred bytes, it must be because this is
	// a retried sync and some blocks snuck in between sync. Those
	// blocks will get transferred now, but they are also on the
	// deferred list and will be retried on the next sync as well.
	df.assimilateDeferredNewBytes()

	// TODO: Returning si.bps in this way is racy, since si is a
	// member of unrefCache.
	return fblock, si.bps, syncState, dirtyDe, nil
}

func prepDirtyEntryForSync(md *RootMetadata, si *syncInfo, dirtyDe *DirEntry) {
	// Add in the cached unref'd blocks.
	si.mergeUnrefCache(md)
	// Update the file's directory entry to the cached copy.
	if dirtyDe != nil {
		dirtyDe.EncodedSize = si.oldInfo.EncodedSize
	}
}

// mergeDirtyEntryWithLBC sets the entry for a file into a directory,
// storing all the affected blocks into `lbc` rather than the dirty
// block cache.  It must only be called with an entry that's already
// been written to the dirty block cache, such that no new blocks are
// dirtied.
func (fbo *folderBlockOps) mergeDirtyEntryWithLBC(
	ctx context.Context, lState *lockState, file path, md KeyMetadata,
	lbc localBcache, dirtyDe DirEntry) error {
	// Lock and fetch for reading only, any dirty blocks will go into
	// the lbc.
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, md)
	if err != nil {
		return err
	}

	dd := fbo.newDirDataWithLBCLocked(
		lState, *file.parentPath(), chargedTo, md, lbc)
	unrefs, err := dd.setEntry(ctx, file.tailName(), dirtyDe)
	if err != nil {
		return err
	}
	if len(unrefs) != 0 {
		return errors.Errorf(
			"Merging dirty entry produced %d new unrefs", len(unrefs))
	}
	return nil
}

// StartSync starts a sync for the given file. It returns the new
// FileBlock which has the readied top-level block which includes all
// writes since the last sync. Must be used with CleanupSyncState()
// and UpdatePointers/FinishSyncLocked() like so:
//
// 	fblock, bps, lbc, syncState, err :=
//		...fbo.StartSync(ctx, lState, md, uid, file)
//	defer func() {
//		...fbo.CleanupSyncState(
//			ctx, lState, md, file, ..., syncState, err)
//	}()
//	if err != nil {
//		...
//	}
//      ...
//
//
//	... = fbo.UpdatePointers(..., func() error {
//      ...fbo.FinishSyncLocked(ctx, lState, file, ..., syncState)
//  })
func (fbo *folderBlockOps) StartSync(ctx context.Context,
	lState *lockState, md *RootMetadata, file path) (
	fblock *FileBlock, bps *blockPutState, dirtyDe *DirEntry,
	syncState fileSyncState, err error) {
	if jServer, err := GetJournalServer(fbo.config); err == nil {
		jServer.dirtyOpStart(fbo.id())
	}

	fblock, bps, syncState, dirtyDe, err = fbo.startSyncWrite(
		ctx, lState, md, file)
	if err != nil {
		return nil, nil, nil, syncState, err
	}

	prepDirtyEntryForSync(md, syncState.si, dirtyDe)
	return fblock, bps, dirtyDe, syncState, err
}

// Does any clean-up for a sync of the given file, given an error
// (which may be nil) that happens during or after StartSync() and
// before FinishSync(). blocksToRemove may be nil.
func (fbo *folderBlockOps) CleanupSyncState(
	ctx context.Context, lState *lockState, md ReadOnlyRootMetadata,
	file path, blocksToRemove []BlockPointer,
	result fileSyncState, err error) {
	if jServer, err := GetJournalServer(fbo.config); err == nil {
		defer jServer.dirtyOpEnd(fbo.id())
	}

	if err == nil {
		return
	}

	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	// Notify error listeners before we reset the dirty blocks and
	// permissions to be granted.
	fbo.notifyErrListenersLocked(lState, file.tailPointer(), err)

	// If there was an error, we need to back out any changes that
	// might have been filled into the sync op, because it could
	// get reused again in a later Sync call.
	if result.si != nil {
		result.si.op.resetUpdateState()

		// Save this MD for later, so we can clean up its
		// newly-referenced block pointers if necessary.
		result.si.toCleanIfUnused = append(result.si.toCleanIfUnused,
			mdToCleanIfUnused{md, result.si.bps.DeepCopy()})
	}
	if isRecoverableBlockError(err) {
		if result.si != nil {
			fbo.revertSyncInfoAfterRecoverableError(blocksToRemove, result)
		}
		if result.fblock != nil {
			result.fblock.Set(result.savedFblock)
			fbo.fixChildBlocksAfterRecoverableErrorLocked(
				ctx, lState, file, md,
				result.redirtyOnRecoverableError)
		}
	} else {
		// Since the sync has errored out unrecoverably, the deferred
		// bytes are already accounted for.
		ds := fbo.deferred[file.tailRef()]
		if df := fbo.dirtyFiles[file.tailPointer()]; df != nil {
			df.updateNotYetSyncingBytes(-ds.waitBytes)

			// Some blocks that were dirty are now clean under their
			// readied block ID, and now live in the bps rather than
			// the dirty bcache, so we can delete them from the dirty
			// bcache.
			dirtyBcache := fbo.config.DirtyBlockCache()
			for _, ptr := range result.oldFileBlockPtrs {
				if df.isBlockOrphaned(ptr) {
					fbo.log.CDebugf(ctx, "Deleting dirty orphan: %v", ptr)
					if err := dirtyBcache.Delete(fbo.id(), ptr,
						fbo.branch()); err != nil {
						fbo.log.CDebugf(ctx, "Couldn't delete %v", ptr)
					}
				}
			}
		}

		// On an unrecoverable error, the deferred writes aren't
		// needed anymore since they're already part of the
		// (still-)dirty blocks.
		delete(fbo.deferred, file.tailRef())
	}

	// The sync is over, due to an error, so reset the map so that we
	// don't defer any subsequent writes.
	// Old syncing blocks are now just dirty
	if df := fbo.dirtyFiles[file.tailPointer()]; df != nil {
		df.resetSyncingBlocksToDirty()
	}
}

// cleanUpUnusedBlocks cleans up the blocks from any previous failed
// sync attempts.
func (fbo *folderBlockOps) cleanUpUnusedBlocks(ctx context.Context,
	md ReadOnlyRootMetadata, syncState fileSyncState, fbm *folderBlockManager) error {
	numToClean := len(syncState.si.toCleanIfUnused)
	if numToClean == 0 {
		return nil
	}

	// What blocks are referenced in the successful MD?
	refs := make(map[BlockPointer]bool)
	for _, op := range md.data.Changes.Ops {
		for _, ptr := range op.Refs() {
			if ptr == zeroPtr {
				panic("Unexpected zero ref ptr in a sync MD revision")
			}
			refs[ptr] = true
		}
		for _, update := range op.allUpdates() {
			if update.Ref == zeroPtr {
				panic("Unexpected zero update ref ptr in a sync MD revision")
			}

			refs[update.Ref] = true
		}
	}

	// For each MD to clean, clean up the old failed blocks
	// immediately if the merge status matches the successful put, if
	// they didn't get referenced in the successful put.  If the merge
	// status is different (e.g., we ended up on a conflict branch),
	// clean it up only if the original revision failed.  If the same
	// block appears more than once, the one with a different merged
	// status takes precedence (which will always come earlier in the
	// list of MDs).
	blocksSeen := make(map[BlockPointer]bool)
	for _, oldMD := range syncState.si.toCleanIfUnused {
		bdType := blockDeleteAlways
		if oldMD.md.MergedStatus() != md.MergedStatus() {
			bdType = blockDeleteOnMDFail
		}

		failedBps := newBlockPutState(len(oldMD.bps.blockStates))
		for _, bs := range oldMD.bps.blockStates {
			if bs.blockPtr == zeroPtr {
				panic("Unexpected zero block ptr in an old sync MD revision")
			}
			if blocksSeen[bs.blockPtr] {
				continue
			}
			blocksSeen[bs.blockPtr] = true
			if refs[bs.blockPtr] && bdType == blockDeleteAlways {
				continue
			}
			failedBps.blockStates = append(failedBps.blockStates,
				blockState{blockPtr: bs.blockPtr})
			fbo.log.CDebugf(ctx, "Cleaning up block %v from a previous "+
				"failed revision %d (oldMD is %s, bdType=%d)", bs.blockPtr,
				oldMD.md.Revision(), oldMD.md.MergedStatus(), bdType)
		}

		if len(failedBps.blockStates) > 0 {
			fbm.cleanUpBlockState(oldMD.md, failedBps, bdType)
		}
	}
	return nil
}

func (fbo *folderBlockOps) doDeferredWritesLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadataWithRootDirEntry, oldPath, newPath path) (
	stillDirty bool, err error) {
	fbo.blockLock.AssertLocked(lState)

	// Redo any writes or truncates that happened to our file while
	// the sync was happening.
	ds := fbo.deferred[oldPath.tailRef()]
	stillDirty = len(ds.writes) != 0
	delete(fbo.deferred, oldPath.tailRef())

	// Clear any dirty blocks that resulted from a write/truncate
	// happening during the sync, since we're redoing them below.
	dirtyBcache := fbo.config.DirtyBlockCache()
	for _, ptr := range ds.dirtyDeletes {
		fbo.log.CDebugf(ctx, "Deleting deferred dirty ptr %v", ptr)
		if err := dirtyBcache.Delete(fbo.id(), ptr, fbo.branch()); err != nil {
			return true, err
		}
	}

	for _, f := range ds.writes {
		err = f(ctx, lState, kmd, newPath)
		if err != nil {
			// It's a little weird to return an error from a deferred
			// write here. Hopefully that will never happen.
			return true, err
		}
	}
	return stillDirty, nil
}

// FinishSyncLocked finishes the sync process for a file, given the
// state from StartSync. Specifically, it re-applies any writes that
// happened since the call to StartSync.
func (fbo *folderBlockOps) FinishSyncLocked(
	ctx context.Context, lState *lockState,
	oldPath, newPath path, md ReadOnlyRootMetadata,
	syncState fileSyncState, fbm *folderBlockManager) (
	stillDirty bool, err error) {
	fbo.blockLock.AssertLocked(lState)

	dirtyBcache := fbo.config.DirtyBlockCache()
	for _, ptr := range syncState.oldFileBlockPtrs {
		fbo.log.CDebugf(ctx, "Deleting dirty ptr %v", ptr)
		if err := dirtyBcache.Delete(fbo.id(), ptr, fbo.branch()); err != nil {
			return true, err
		}
	}

	bcache := fbo.config.BlockCache()
	for _, ptr := range syncState.newIndirectFileBlockPtrs {
		err := bcache.DeletePermanent(ptr.ID)
		if err != nil {
			fbo.log.CWarningf(ctx, "Error when deleting %v from cache: %v",
				ptr.ID, err)
		}
	}

	stillDirty, err = fbo.doDeferredWritesLocked(
		ctx, lState, md, oldPath, newPath)
	if err != nil {
		return true, err
	}

	// Clear cached info for the old path.  We are guaranteed that any
	// concurrent write to this file was deferred, even if it was to a
	// block that wasn't currently being sync'd, since the top-most
	// block is always in dirtyFiles and is always dirtied during a
	// write/truncate.
	//
	// Also, we can get rid of all the sync state that might have
	// happened during the sync, since we will replay the writes
	// below anyway.
	if err := fbo.clearCacheInfoLocked(lState, oldPath); err != nil {
		return true, err
	}

	if err := fbo.cleanUpUnusedBlocks(ctx, md, syncState, fbm); err != nil {
		return true, err
	}

	return stillDirty, nil
}

// notifyErrListeners notifies any write operations that are blocked
// on a file so that they can learn about unrecoverable sync errors.
func (fbo *folderBlockOps) notifyErrListenersLocked(lState *lockState,
	ptr BlockPointer, err error) {
	fbo.blockLock.AssertLocked(lState)
	if isRecoverableBlockError(err) {
		// Don't bother any listeners with this error, since the sync
		// will be retried.  Unless the sync has reached its retry
		// limit, but in that case the listeners will just proceed as
		// normal once the dirty block cache bytes are freed, and
		// that's ok since this error isn't fatal.
		return
	}
	df := fbo.dirtyFiles[ptr]
	if df != nil {
		df.notifyErrListeners(err)
	}
}

type searchWithOutOfDateCacheError struct {
}

func (e searchWithOutOfDateCacheError) Error() string {
	return fmt.Sprintf("Search is using an out-of-date node cache; " +
		"try again with a clean cache.")
}

// searchForNodesInDirLocked recursively tries to find a path, and
// ultimately a node, to ptr, given the set of pointers that were
// updated in a particular operation.  The keys in nodeMap make up the
// set of BlockPointers that are being searched for, and nodeMap is
// updated in place to include the corresponding discovered nodes.
//
// Returns the number of nodes found by this invocation.  If the error
// it returns is searchWithOutOfDateCache, the search should be
// retried by the caller with a clean cache.
func (fbo *folderBlockOps) searchForNodesInDirLocked(ctx context.Context,
	lState *lockState, cache NodeCache, newPtrs map[BlockPointer]bool,
	kmd KeyMetadata, rootNode Node, currDir path, nodeMap map[BlockPointer]Node,
	numNodesFoundSoFar int) (int, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return 0, err
	}
	dd := fbo.newDirDataLocked(lState, currDir, chargedTo, kmd)
	entries, err := dd.getEntries(ctx)
	if err != nil {
		return 0, err
	}

	// getDirLocked may have unlocked blockLock, which means the cache
	// could have changed out from under us.  Verify that didn't
	// happen, so we can avoid messing it up with nodes from an old MD
	// version.  If it did happen, return a special error that lets
	// the caller know they should retry with a fresh cache.
	if currDir.path[0].BlockPointer !=
		cache.PathFromNode(rootNode).tailPointer() {
		return 0, searchWithOutOfDateCacheError{}
	}

	if numNodesFoundSoFar >= len(nodeMap) {
		return 0, nil
	}

	numNodesFound := 0
	for name, de := range entries {
		if _, ok := nodeMap[de.BlockPointer]; ok {
			childPath := currDir.ChildPath(name, de.BlockPointer)
			// make a node for every pathnode
			n := rootNode
			for i, pn := range childPath.path[1:] {
				if !pn.BlockPointer.IsValid() {
					// Temporary debugging output for KBFS-1764 -- the
					// GetOrCreate call below will panic.
					fbo.log.CDebugf(ctx, "Invalid block pointer, path=%s, "+
						"path.path=%v (index %d), name=%s, de=%#v, "+
						"nodeMap=%v, newPtrs=%v, kmd=%#v",
						childPath, childPath.path, i, name, de, nodeMap,
						newPtrs, kmd)
				}
				n, err = cache.GetOrCreate(pn.BlockPointer, pn.Name, n)
				if err != nil {
					return 0, err
				}
			}
			nodeMap[de.BlockPointer] = n
			numNodesFound++
			if numNodesFoundSoFar+numNodesFound >= len(nodeMap) {
				return numNodesFound, nil
			}
		}

		// otherwise, recurse if this represents an updated block
		if _, ok := newPtrs[de.BlockPointer]; de.Type == Dir && ok {
			childPath := currDir.ChildPath(name, de.BlockPointer)
			n, err := fbo.searchForNodesInDirLocked(ctx, lState, cache,
				newPtrs, kmd, rootNode, childPath, nodeMap,
				numNodesFoundSoFar+numNodesFound)
			if err != nil {
				return 0, err
			}
			numNodesFound += n
			if numNodesFoundSoFar+numNodesFound >= len(nodeMap) {
				return numNodesFound, nil
			}
		}
	}

	return numNodesFound, nil
}

func (fbo *folderBlockOps) trySearchWithCacheLocked(ctx context.Context,
	lState *lockState, cache NodeCache, ptrs []BlockPointer,
	newPtrs map[BlockPointer]bool, kmd KeyMetadata, rootPtr BlockPointer) (
	map[BlockPointer]Node, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	nodeMap := make(map[BlockPointer]Node)
	for _, ptr := range ptrs {
		nodeMap[ptr] = nil
	}

	if len(ptrs) == 0 {
		return nodeMap, nil
	}

	var node Node
	// The node cache used by the main part of KBFS is
	// fbo.nodeCache. This basically maps from BlockPointers to
	// Nodes. Nodes are used by the callers of the library, but
	// internally we need to know the series of BlockPointers and
	// file/dir names that make up the path of the corresponding
	// file/dir. fbo.nodeCache is long-lived and never invalidated.
	//
	// As folderBranchOps gets informed of new local or remote MD
	// updates, which change the BlockPointers of some subset of the
	// nodes in this TLF, it calls nodeCache.UpdatePointer for each
	// change. Then, when a caller passes some old Node they have
	// lying around into an FBO call, we can translate it to its
	// current path using fbo.nodeCache. Note that on every TLF
	// modification, we are guaranteed that the BlockPointer of the
	// root directory will change (because of the merkle-ish tree of
	// content hashes we use to assign BlockPointers).
	//
	// fbo.nodeCache needs to maintain the absolute latest mappings
	// for the TLF, or else FBO calls won't see up-to-date data. The
	// tension in search comes from the fact that we are trying to
	// discover the BlockPointers of certain files at a specific point
	// in the MD history, which is not necessarily the same as the
	// most-recently-seen MD update. Specifically, some callers
	// process a specific range of MDs, but folderBranchOps may have
	// heard about a newer one before, or during, when the caller
	// started processing. That means fbo.nodeCache may have been
	// updated to reflect the newest BlockPointers, and is no longer
	// correct as a cache for our search for the data at the old point
	// in time.
	if cache == fbo.nodeCache {
		// Root node should already exist if we have an up-to-date md.
		node = cache.Get(rootPtr.Ref())
		if node == nil {
			return nil, searchWithOutOfDateCacheError{}
		}
	} else {
		// Root node may or may not exist.
		var err error
		node, err = cache.GetOrCreate(rootPtr,
			string(kmd.GetTlfHandle().GetCanonicalName()), nil)
		if err != nil {
			return nil, err
		}
	}
	if node == nil {
		return nil, fmt.Errorf("Cannot find root node corresponding to %v",
			rootPtr)
	}

	// are they looking for the root directory?
	numNodesFound := 0
	if _, ok := nodeMap[rootPtr]; ok {
		nodeMap[rootPtr] = node
		numNodesFound++
		if numNodesFound >= len(nodeMap) {
			return nodeMap, nil
		}
	}

	rootPath := cache.PathFromNode(node)
	if len(rootPath.path) != 1 {
		return nil, fmt.Errorf("Invalid root path for %v: %s",
			rootPtr, rootPath)
	}

	_, err := fbo.searchForNodesInDirLocked(ctx, lState, cache, newPtrs,
		kmd, node, rootPath, nodeMap, numNodesFound)
	if err != nil {
		return nil, err
	}

	if rootPtr != cache.PathFromNode(node).tailPointer() {
		return nil, searchWithOutOfDateCacheError{}
	}

	return nodeMap, nil
}

func (fbo *folderBlockOps) searchForNodesLocked(ctx context.Context,
	lState *lockState, cache NodeCache, ptrs []BlockPointer,
	newPtrs map[BlockPointer]bool, kmd KeyMetadata, rootPtr BlockPointer) (
	map[BlockPointer]Node, NodeCache, error) {
	fbo.blockLock.AssertAnyLocked(lState)

	// First try the passed-in cache.  If it doesn't work because the
	// cache is out of date, try again with a clean cache.
	nodeMap, err := fbo.trySearchWithCacheLocked(ctx, lState, cache, ptrs,
		newPtrs, kmd, rootPtr)
	if _, ok := err.(searchWithOutOfDateCacheError); ok {
		// The md is out-of-date, so use a throwaway cache so we
		// don't pollute the real node cache with stale nodes.
		fbo.log.CDebugf(ctx, "Root node %v doesn't exist in the node "+
			"cache; using a throwaway node cache instead",
			rootPtr)
		cache = newNodeCacheStandard(fbo.folderBranch)
		nodeMap, err = fbo.trySearchWithCacheLocked(ctx, lState, cache, ptrs,
			newPtrs, kmd, rootPtr)
	}

	if err != nil {
		return nil, nil, err
	}

	// Return the whole map even if some nodes weren't found.
	return nodeMap, cache, nil
}

// SearchForNodes tries to resolve all the given pointers to a Node
// object, using only the updated pointers specified in newPtrs.
// Returns an error if any subset of the pointer paths do not exist;
// it is the caller's responsibility to decide to error on particular
// unresolved nodes.  It also returns the cache that ultimately
// contains the nodes -- this might differ from the passed-in cache if
// another goroutine updated that cache and it no longer contains the
// root pointer specified in md.
func (fbo *folderBlockOps) SearchForNodes(ctx context.Context,
	cache NodeCache, ptrs []BlockPointer, newPtrs map[BlockPointer]bool,
	kmd KeyMetadata, rootPtr BlockPointer) (
	map[BlockPointer]Node, NodeCache, error) {
	lState := makeFBOLockState()
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	return fbo.searchForNodesLocked(
		ctx, lState, cache, ptrs, newPtrs, kmd, rootPtr)
}

// SearchForPaths is like SearchForNodes, except it returns a
// consistent view of all the paths of the searched-for pointers.
func (fbo *folderBlockOps) SearchForPaths(ctx context.Context,
	cache NodeCache, ptrs []BlockPointer, newPtrs map[BlockPointer]bool,
	kmd KeyMetadata, rootPtr BlockPointer) (map[BlockPointer]path, error) {
	lState := makeFBOLockState()
	// Hold the lock while processing the paths so they can't be changed.
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	nodeMap, cache, err :=
		fbo.searchForNodesLocked(
			ctx, lState, cache, ptrs, newPtrs, kmd, rootPtr)
	if err != nil {
		return nil, err
	}

	paths := make(map[BlockPointer]path)
	for ptr, n := range nodeMap {
		if n == nil {
			paths[ptr] = path{}
			continue
		}

		p := cache.PathFromNode(n)
		if p.tailPointer() != ptr {
			return nil, NodeNotFoundError{ptr}
		}
		paths[ptr] = p
	}

	return paths, nil
}

// UpdateCachedEntryAttributesOnRemovedFile updates any cached entry
// for the given path of an unlinked file, according to the given op,
// and it makes a new dirty cache entry if one doesn't exist yet.  We
// assume Sync will be called eventually on the corresponding open
// file handle, which will clear out the entry.
func (fbo *folderBlockOps) UpdateCachedEntryAttributesOnRemovedFile(
	ctx context.Context, lState *lockState, kmd KeyMetadataWithRootDirEntry,
	op *setAttrOp, p path, de DirEntry) error {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	_, err := fbo.setCachedAttrLocked(
		ctx, lState, kmd, *p.parentPath(), p.tailName(), op.Attr, de)
	return err
}

func (fbo *folderBlockOps) getDeferredWriteCountForTest(lState *lockState) int {
	fbo.blockLock.RLock(lState)
	defer fbo.blockLock.RUnlock(lState)
	writes := 0
	for _, ds := range fbo.deferred {
		writes += len(ds.writes)
	}
	return writes
}

func (fbo *folderBlockOps) updatePointer(kmd KeyMetadata, oldPtr BlockPointer, newPtr BlockPointer, shouldPrefetch bool) NodeID {
	updatedNode := fbo.nodeCache.UpdatePointer(oldPtr.Ref(), newPtr)
	if updatedNode == nil || oldPtr.ID == newPtr.ID {
		return nil
	}

	// Only prefetch if the updated pointer is a new block ID.
	// TODO: Remove this comment when we're done debugging because it'll be everywhere.
	ctx := context.TODO()
	fbo.log.CDebugf(ctx, "Updated reference for pointer %s to %s.", oldPtr.ID, newPtr.ID)
	if shouldPrefetch {
		// Prefetch the new ref, but only if the old ref already exists in
		// the block cache. Ideally we'd always prefetch it, but we need
		// the type of the block so that we can call `NewEmpty`.
		block, _, lifetime, err :=
			fbo.config.BlockCache().GetWithPrefetch(oldPtr)
		if err != nil {
			return updatedNode
		}

		// No need to cache because it's already cached.
		_ = fbo.config.BlockOps().BlockRetriever().Request(ctx,
			updatePointerPrefetchPriority, kmd, newPtr, block.NewEmpty(),
			lifetime)
	}
	// Cancel any prefetches for the old pointer from the prefetcher.
	fbo.config.BlockOps().Prefetcher().CancelPrefetch(oldPtr.ID)
	return updatedNode
}

// UpdatePointers updates all the pointers in the node cache
// atomically.  If `afterUpdateFn` is non-nil, it's called under the
// same block lock under which the pointers were updated.
func (fbo *folderBlockOps) UpdatePointers(kmd KeyMetadata, lState *lockState,
	op op, shouldPrefetch bool, afterUpdateFn func() error) (
	affectedNodeIDs []NodeID, err error) {
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)
	for _, update := range op.allUpdates() {
		updatedNode := fbo.updatePointer(
			kmd, update.Unref, update.Ref, shouldPrefetch)
		if updatedNode != nil {
			affectedNodeIDs = append(affectedNodeIDs, updatedNode)
		}
	}

	if afterUpdateFn == nil {
		return affectedNodeIDs, nil
	}

	return affectedNodeIDs, afterUpdateFn()
}

func (fbo *folderBlockOps) unlinkDuringFastForwardLocked(ctx context.Context,
	lState *lockState, kmd KeyMetadataWithRootDirEntry, ref BlockRef) {
	fbo.blockLock.AssertLocked(lState)
	oldNode := fbo.nodeCache.Get(ref)
	if oldNode == nil {
		return
	}
	oldPath := fbo.nodeCache.PathFromNode(oldNode)
	fbo.log.CDebugf(ctx, "Unlinking missing node %s/%v during "+
		"fast-forward", oldPath, ref)
	de, err := fbo.getEntryLocked(ctx, lState, kmd, oldPath, true)
	if err != nil {
		fbo.log.CDebugf(ctx, "Couldn't find old dir entry for %s/%v: %+v",
			oldPath, ref, err)
	}
	fbo.nodeCache.Unlink(ref, oldPath, de)
}

func (fbo *folderBlockOps) fastForwardDirAndChildrenLocked(ctx context.Context,
	lState *lockState, currDir path, children map[string]map[pathNode]bool,
	kmd KeyMetadataWithRootDirEntry) (
	changes []NodeChange, affectedNodeIDs []NodeID, err error) {
	fbo.blockLock.AssertLocked(lState)

	chargedTo, err := fbo.getChargedToLocked(ctx, lState, kmd)
	if err != nil {
		return nil, nil, err
	}
	dd := fbo.newDirDataLocked(lState, currDir, chargedTo, kmd)
	entries, err := dd.getEntries(ctx)
	if err != nil {
		return nil, nil, err
	}

	prefix := currDir.String()

	// TODO: parallelize me?
	for child := range children[prefix] {
		entry, ok := entries[child.Name]
		if !ok {
			fbo.unlinkDuringFastForwardLocked(
				ctx, lState, kmd, child.BlockPointer.Ref())
			continue
		}

		fbo.log.CDebugf(ctx, "Fast-forwarding %v -> %v",
			child.BlockPointer, entry.BlockPointer)
		fbo.updatePointer(kmd, child.BlockPointer,
			entry.BlockPointer, true)
		node := fbo.nodeCache.Get(entry.BlockPointer.Ref())
		newPath := fbo.nodeCache.PathFromNode(node)
		if entry.Type == Dir {
			if node != nil {
				change := NodeChange{Node: node}
				for subchild := range children[newPath.String()] {
					change.DirUpdated = append(change.DirUpdated, subchild.Name)
				}
				changes = append(changes, change)
				affectedNodeIDs = append(affectedNodeIDs, node.GetID())
			}

			childChanges, childAffectedNodeIDs, err :=
				fbo.fastForwardDirAndChildrenLocked(
					ctx, lState, newPath, children, kmd)
			if err != nil {
				return nil, nil, err
			}
			changes = append(changes, childChanges...)
			affectedNodeIDs = append(affectedNodeIDs, childAffectedNodeIDs...)
		} else if node != nil {
			// File -- invalidate the entire file contents.
			changes = append(changes, NodeChange{
				Node:        node,
				FileUpdated: []WriteRange{{Len: 0, Off: 0}},
			})
			affectedNodeIDs = append(affectedNodeIDs, node.GetID())
		}
	}
	delete(children, prefix)
	return changes, affectedNodeIDs, nil
}

// FastForwardAllNodes attempts to update the block pointers
// associated with nodes in the cache by searching for their paths in
// the current version of the TLF.  If it can't find a corresponding
// node, it assumes it's been deleted and unlinks it.  Returns the set
// of node changes that resulted.  If there are no nodes, it returns a
// nil error because there's nothing to be done.
func (fbo *folderBlockOps) FastForwardAllNodes(ctx context.Context,
	lState *lockState, md ReadOnlyRootMetadata) (
	changes []NodeChange, affectedNodeIDs []NodeID, err error) {
	if fbo.nodeCache == nil {
		// Nothing needs to be done!
		return nil, nil, nil
	}

	// Take a hard lock through this whole process.  TODO: is there
	// any way to relax this?  It could lead to file system operation
	// timeouts, even on reads, if we hold it too long.
	fbo.blockLock.Lock(lState)
	defer fbo.blockLock.Unlock(lState)

	nodes := fbo.nodeCache.AllNodes()
	if len(nodes) == 0 {
		// Nothing needs to be done!
		return nil, nil, nil
	}
	fbo.log.CDebugf(ctx, "Fast-forwarding %d nodes", len(nodes))
	defer func() { fbo.log.CDebugf(ctx, "Fast-forward complete: %v", err) }()

	// Build a "tree" representation for each interesting path prefix.
	children := make(map[string]map[pathNode]bool)
	var rootPath path
	for _, n := range nodes {
		p := fbo.nodeCache.PathFromNode(n)
		if len(p.path) == 1 {
			rootPath = p
		}
		prevPath := ""
		for _, pn := range p.path {
			if prevPath != "" {
				childPNs := children[prevPath]
				if childPNs == nil {
					childPNs = make(map[pathNode]bool)
					children[prevPath] = childPNs
				}
				childPNs[pn] = true
			}
			prevPath = filepath.Join(prevPath, pn.Name)
		}
	}

	if !rootPath.isValid() {
		return nil, nil, errors.New("Couldn't find the root path")
	}

	fbo.log.CDebugf(ctx, "Fast-forwarding root %v -> %v",
		rootPath.path[0].BlockPointer, md.data.Dir.BlockPointer)
	fbo.updatePointer(md, rootPath.path[0].BlockPointer,
		md.data.Dir.BlockPointer, false)
	rootPath.path[0].BlockPointer = md.data.Dir.BlockPointer
	rootNode := fbo.nodeCache.Get(md.data.Dir.BlockPointer.Ref())
	if rootNode != nil {
		change := NodeChange{Node: rootNode}
		for child := range children[rootPath.String()] {
			change.DirUpdated = append(change.DirUpdated, child.Name)
		}
		changes = append(changes, change)
		affectedNodeIDs = append(affectedNodeIDs, rootNode.GetID())
	}

	childChanges, childAffectedNodeIDs, err :=
		fbo.fastForwardDirAndChildrenLocked(
			ctx, lState, rootPath, children, md)
	if err != nil {
		return nil, nil, err
	}
	changes = append(changes, childChanges...)
	affectedNodeIDs = append(affectedNodeIDs, childAffectedNodeIDs...)

	// Unlink any children that remain.
	for _, childPNs := range children {
		for child := range childPNs {
			fbo.unlinkDuringFastForwardLocked(
				ctx, lState, md, child.BlockPointer.Ref())
		}
	}
	return changes, affectedNodeIDs, nil
}

type chainsPathPopulator interface {
	populateChainPaths(context.Context, logger.Logger, *crChains, bool) error
}

// populateChainPaths updates all the paths in all the ops tracked by
// `chains`, using the main nodeCache.
func (fbo *folderBlockOps) populateChainPaths(ctx context.Context,
	log logger.Logger, chains *crChains, includeCreates bool) error {
	_, err := chains.getPaths(ctx, fbo, log, fbo.nodeCache, includeCreates)
	return err
}

var _ chainsPathPopulator = (*folderBlockOps)(nil)
