// Copyright 2017 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"path/filepath"

	"github.com/keybase/kbfs/kbfsblock"
	"github.com/keybase/kbfs/kbfscrypto"
	"github.com/keybase/kbfs/tlf"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"golang.org/x/net/context"
)

const (
	workingSetCacheFolderName = "kbfs_block_cache"
	syncCacheFolderName       = "kbfs_sync_cache"
)

// diskBlockCacheConfig specifies the interfaces that a DiskBlockCacheStandard
// needs to perform its functions. This adheres to the standard libkbfs Config
// API.
type diskBlockCacheConfig interface {
	codecGetter
	logMaker
	clockGetter
	diskLimiterGetter
	syncedTlfGetterSetter
}

type diskBlockCacheWrapped struct {
	config          diskBlockCacheConfig
	storageRoot     string
	workingSetCache DiskBlockCache
	syncCache       DiskBlockCache
}

var _ DiskBlockCache = (*diskBlockCacheWrapped)(nil)

func newDiskBlockCacheWrapped(config diskBlockCacheConfig, storageRoot string) (
	cache *diskBlockCacheWrapped, err error) {
	workingSetCacheRoot := filepath.Join(storageRoot, workingSetCacheFolderName)
	workingSetCache, err := newDiskBlockCacheStandard(config,
		workingSetCacheLimitTrackerType, workingSetCacheRoot)
	if err != nil {
		return nil, err
	}
	cache = &diskBlockCacheWrapped{
		config:          config,
		storageRoot:     storageRoot,
		workingSetCache: workingSetCache,
		syncCache:       nil,
	}
	// TODO: enable sync cache in a subsequent PR.
	// _ = cache.enableSyncCache()
	return cache, nil
}

func (cache *diskBlockCacheWrapped) enableSyncCache() (err error) {
	if cache.syncCache != nil {
		return nil
	}
	syncCacheRoot := filepath.Join(cache.storageRoot, syncCacheFolderName)
	cache.syncCache, err = newDiskBlockCacheStandard(cache.config,
		syncCacheLimitTrackerType, syncCacheRoot)
	if err != nil {
		return err
	}
	return nil
}

// Get implements the DiskBlockCache interface for diskBlockCacheWrapped.
func (cache *diskBlockCacheWrapped) Get(ctx context.Context, tlfID tlf.ID,
	blockID kbfsblock.ID) (
	buf []byte, serverHalf kbfscrypto.BlockCryptKeyServerHalf,
	hasPrefetched bool, err error) {
	// TODO: add mutex to guard sync state
	primaryCache := cache.workingSetCache
	secondaryCache := cache.syncCache
	if cache.config.IsSyncedTlf(tlfID) && cache.syncCache != nil {
		primaryCache, secondaryCache = secondaryCache, primaryCache
	}
	// Check both caches if the primary cache doesn't have the block.
	buf, serverHalf, hasPrefetched, err = primaryCache.Get(ctx, tlfID, blockID)
	if _, isNoSuchBlockError := err.(NoSuchBlockError); isNoSuchBlockError &&
		secondaryCache != nil {
		return secondaryCache.Get(ctx, tlfID, blockID)
	}
	return buf, serverHalf, hasPrefetched, err
}

// GetMetadata implements the DiskBlockCache interface for
// diskBlockCacheWrapped.
func (cache *diskBlockCacheWrapped) GetMetadata(ctx context.Context,
	blockID kbfsblock.ID) (metadata DiskBlockCacheMetadata, err error) {
	// TODO: add mutex to guard sync state
	if cache.syncCache != nil {
		md, err := cache.syncCache.GetMetadata(ctx, blockID)
		switch err {
		case nil:
			return md, nil
		case errors.ErrNotFound:
		default:
			return md, err
		}
	}
	return cache.workingSetCache.GetMetadata(ctx, blockID)
}

// Put implements the DiskBlockCache interface for diskBlockCacheWrapped.
func (cache *diskBlockCacheWrapped) Put(ctx context.Context, tlfID tlf.ID,
	blockID kbfsblock.ID, buf []byte,
	serverHalf kbfscrypto.BlockCryptKeyServerHalf) error {
	if cache.config.IsSyncedTlf(tlfID) && cache.syncCache != nil {
		go cache.workingSetCache.Delete(ctx, []kbfsblock.ID{blockID})
		return cache.syncCache.Put(ctx, tlfID, blockID, buf, serverHalf)
	}
	// TODO: Allow more intelligent transitioning from the sync cache to
	// the working set cache.
	if cache.syncCache != nil {
		go cache.syncCache.Delete(ctx, []kbfsblock.ID{blockID})
	}
	return cache.workingSetCache.Put(ctx, tlfID, blockID, buf, serverHalf)
}

// Delete implements the DiskBlockCache interface for diskBlockCacheWrapped.
func (cache *diskBlockCacheWrapped) Delete(ctx context.Context,
	blockIDs []kbfsblock.ID) (numRemoved int, sizeRemoved int64, err error) {
	numRemoved, sizeRemoved, err = cache.workingSetCache.Delete(ctx, blockIDs)
	if cache.syncCache == nil || err != nil {
		return numRemoved, sizeRemoved, err
	}
	syncNumRemoved, syncSizeRemoved, err :=
		cache.syncCache.Delete(ctx, blockIDs)
	return numRemoved + syncNumRemoved, sizeRemoved + syncSizeRemoved, err
}

// UpdateMetadata implements the DiskBlockCache interface for
// diskBlockCacheWrapped.
func (cache *diskBlockCacheWrapped) UpdateMetadata(ctx context.Context,
	blockID kbfsblock.ID, hasPrefetched, donePrefetch bool) error {
	// Try to update metadata for both caches.
	if cache.syncCache != nil {
		err := cache.syncCache.UpdateMetadata(ctx, blockID, hasPrefetched,
			donePrefetch)
		_, isNoSuchBlockError := err.(NoSuchBlockError)
		if !isNoSuchBlockError {
			return err
		}
	}
	return cache.workingSetCache.UpdateMetadata(ctx, blockID, hasPrefetched,
		donePrefetch)
}

// Size implements the DiskBlockCache interface for diskBlockCacheWrapped.
func (cache *diskBlockCacheWrapped) Size() int64 {
	size := cache.workingSetCache.Size()
	if cache.syncCache != nil {
		size += cache.syncCache.Size()
	}
	return size
}

// Status implements the DiskBlockCache interface for diskBlockCacheWrapped.
func (cache *diskBlockCacheWrapped) Status(
	ctx context.Context) map[string]DiskBlockCacheStatus {
	statuses := make(map[string]DiskBlockCacheStatus, 2)
	for name, status := range cache.workingSetCache.Status(ctx) {
		statuses[name] = status
	}
	if cache.syncCache == nil {
		return statuses
	}
	for name, status := range cache.syncCache.Status(ctx) {
		statuses[name] = status
	}
	return statuses
}

// Shutdown implements the DiskBlockCache interface for diskBlockCacheWrapped.
func (cache *diskBlockCacheWrapped) Shutdown(ctx context.Context) {
	cache.workingSetCache.Shutdown(ctx)
	if cache.syncCache != nil {
		cache.syncCache.Shutdown(ctx)
	}
}
