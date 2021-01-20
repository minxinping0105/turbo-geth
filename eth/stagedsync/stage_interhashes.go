package stagedsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/changeset"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/common/etl"
	"github.com/ledgerwatch/turbo-geth/core/rawdb"
	"github.com/ledgerwatch/turbo-geth/eth/stagedsync/stages"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/turbo/shards"
	"github.com/ledgerwatch/turbo-geth/turbo/trie"
)

func SpawnIntermediateHashesStage(s *StageState, db ethdb.Database, checkRoot bool, cache *shards.StateCache, tmpdir string, quit <-chan struct{}) error {
	to, err := s.ExecutionAt(db)
	if err != nil {
		return err
	}

	if s.BlockNumber == to {
		// we already did hash check for this block
		// we don't do the obvious `if s.BlockNumber > to` to support reorgs more naturally
		s.Done()
		return nil
	}
	fmt.Printf("%d->%d\n", s.BlockNumber, to)

	var tx ethdb.DbWithPendingMutations
	var useExternalTx bool
	if hasTx, ok := db.(ethdb.HasTx); ok && hasTx.Tx() != nil {
		tx = db.(ethdb.DbWithPendingMutations)
		useExternalTx = true
	} else {
		tx, err = db.Begin(context.Background(), ethdb.RW)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	hash, err := rawdb.ReadCanonicalHash(tx, to)
	if err != nil {
		return err
	}
	syncHeadHeader := rawdb.ReadHeader(tx, hash, to)
	expectedRootHash := syncHeadHeader.Root

	logPrefix := s.state.LogPrefix()
	log.Info(fmt.Sprintf("[%s] Generating intermediate hashes", logPrefix), "from", s.BlockNumber, "to", to)
	if s.BlockNumber == 0 {
		if err := RegenerateIntermediateHashes(logPrefix, tx, checkRoot, cache, tmpdir, expectedRootHash, quit); err != nil {
			return err
		}
	} else {
		if err := incrementIntermediateHashes(logPrefix, s, tx, to, checkRoot, cache, tmpdir, expectedRootHash, quit); err != nil {
			return err
		}
	}

	if err := s.DoneAndUpdate(tx, to); err != nil {
		return err
	}

	if !useExternalTx {
		if _, err := tx.Commit(); err != nil {
			return err
		}
	}

	return nil
}

func RegenerateIntermediateHashes(logPrefix string, db ethdb.Database, checkRoot bool, cache *shards.StateCache, tmpdir string, expectedRootHash common.Hash, quit <-chan struct{}) error {
	log.Info(fmt.Sprintf("[%s] Regeneration intermediate hashes started", logPrefix))
	// Clear IH bucket
	c := db.(ethdb.HasTx).Tx().Cursor(dbutils.IntermediateHashOfAccountBucket)
	for k, _, err := c.First(); k != nil; k, _, err = c.First() {
		if err != nil {
			return err
		}
		if err = c.DeleteCurrent(); err != nil {
			return err
		}
	}
	c.Close()
	c = db.(ethdb.HasTx).Tx().Cursor(dbutils.IntermediateHashOfStorageBucket)
	for k, _, err := c.First(); k != nil; k, _, err = c.First() {
		if err != nil {
			return err
		}
		if err = c.DeleteCurrent(); err != nil {
			return err
		}
	}
	c.Close()

	if cache != nil {
		for i := 0; i < 16; i++ {
			unfurl := trie.NewRetainList(0)
			newV := make([]common.Hash, 0, 17)
			hashCollector := func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
				if len(keyHex) == 0 {
					return nil
				}
				if hashes == nil {
					cache.SetAccountHashDelete(keyHex)
					return nil
				}
				newV = newV[:len(hashes)/common.HashLength+len(rootHash)/common.HashLength]
				copyTo := newV
				if len(rootHash) > 0 {
					newV[0].SetBytes(rootHash)
					copyTo = newV[0:]
				}
				for j := 0; j < len(copyTo); j++ {
					copyTo[j].SetBytes(hashes[j*common.HashLength : (j+1)*common.HashLength])
				}
				cache.SetAccountHashWrite(keyHex, branchSet, set, newV)
				return nil
			}
			storageHashCollector := func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
				addr, inc := common.BytesToHash(accWithInc[:32]), binary.BigEndian.Uint64(accWithInc[32:])
				if hashes == nil {
					cache.SetStorageHashDelete(addr, inc, keyHex, branchSet, set, nil)
					return nil
				}
				newV = newV[:len(hashes)/common.HashLength+len(rootHash)/common.HashLength]
				copyTo := newV
				if len(rootHash) > 0 {
					newV[0].SetBytes(rootHash)
					copyTo = newV[0:]
				}
				for j := 0; j < len(copyTo); j++ {
					copyTo[j].SetBytes(hashes[j*common.HashLength : (j+1)*common.HashLength])
				}
				cache.SetStorageHashWrite(addr, inc, keyHex, branchSet, set, newV)
				return nil
			}
			loader := trie.NewFlatDBTrieLoader(logPrefix)
			// hashCollector in the line below will collect deletes
			if err := loader.Reset(unfurl, hashCollector, storageHashCollector, false); err != nil {
				return err
			}
			_, err := loader.CalcTrieRootOnCache(db, []byte{uint8(i)}, cache, quit)
			if err != nil {
				return err
			}
		}
		loader := trie.NewFlatDBTrieLoader(logPrefix)
		if err := loader.Reset(trie.NewRetainList(0), func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			return nil
		}, func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			return nil
		}, false); err != nil {
			return err
		}
		t := time.Now()
		hash, err := loader.CalcTrieRootOnCache2(cache)
		if err != nil {
			return err
		}
		generationIHTook := time.Since(t)
		if checkRoot && hash != expectedRootHash {
			return fmt.Errorf("%s: wrong trie root: %x, expected (from header): %x", logPrefix, hash, expectedRootHash)
		}
		log.Info(fmt.Sprintf("[%s] Collection finished", logPrefix),
			"root hash", hash.Hex(),
			"gen IH", generationIHTook,
		)
		_ = cache.DebugPrintAccounts()
		writes := cache.PrepareWrites()

		shards.WalkAccountHashesWrites(writes, func(prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newV := make([]byte, len(h)*common.HashLength+4)
			binary.BigEndian.PutUint16(newV, branchChildren)
			binary.BigEndian.PutUint16(newV[2:], children)
			for i := 0; i < len(h); i++ {
				copy(newV[4+i*common.HashLength:4+(i+1)*common.HashLength], h[i].Bytes())
			}
			if err := db.Put(dbutils.IntermediateHashOfAccountBucket, prefix, newV); err != nil {
				panic(err)
			}
		}, func(prefix []byte, branchChildren, children uint16, h []common.Hash) {
			if err := db.Delete(dbutils.IntermediateHashOfAccountBucket, prefix, nil); err != nil {
				panic(err)
			}
		})
		shards.WalkStorageHashesWrites(writes, func(addrHash common.Hash, incarnation uint64, prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newV := make([]byte, len(h)*common.HashLength+4)
			binary.BigEndian.PutUint16(newV, branchChildren)
			binary.BigEndian.PutUint16(newV[2:], children)
			for i := 0; i < len(h); i++ {
				copy(newV[4+i*common.HashLength:4+(i+1)*common.HashLength], h[i].Bytes())
			}
			newK := dbutils.GenerateCompositeStoragePrefix(addrHash.Bytes(), incarnation, prefix)
			if err := db.Put(dbutils.IntermediateHashOfStorageBucket, newK, newV); err != nil {
				panic(err)
			}
		}, func(addrHash common.Hash, incarnation uint64, prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newK := dbutils.GenerateCompositeStoragePrefix(addrHash.Bytes(), incarnation, prefix)
			if err := db.Delete(dbutils.IntermediateHashOfStorageBucket, newK, nil); err != nil {
				panic(err)
			}
		})
		cache.TurnWritesToReads(writes)
	} else {
		accountIHCollector := etl.NewCollector(tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
		storageIHCollector := etl.NewCollector(tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
		newV := make([]byte, 0, 1024)
		hashCollector := func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			if len(keyHex) == 0 {
				return nil
			}
			if hashes == nil {
				//fmt.Printf("collect del: %x\n", keyHex)
				return accountIHCollector.Collect(keyHex, nil)
			}
			newV = newV[:len(hashes)+len(rootHash)+4]
			binary.BigEndian.PutUint16(newV, branchSet)
			binary.BigEndian.PutUint16(newV[2:], set)
			if len(rootHash) == 0 {
				copy(newV[4:], hashes)
			} else {
				copy(newV[4:], rootHash)
				copy(newV[36:], hashes)
			}
			//fmt.Printf("collect write: %x, %016b\n", keyHex, branchSet)
			return accountIHCollector.Collect(keyHex, newV)
		}
		newK := make([]byte, 0, 128)
		storageHashCollector := func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			newK = append(append(newK[:0], accWithInc...), keyHex...)
			if hashes == nil {
				return storageIHCollector.Collect(newK, nil)
			}
			newV = newV[:len(hashes)+len(rootHash)+4]
			binary.BigEndian.PutUint16(newV, branchSet)
			binary.BigEndian.PutUint16(newV[2:], set)
			if len(rootHash) == 0 {
				copy(newV[4:], hashes)
			} else {
				copy(newV[4:], rootHash)
				copy(newV[36:], hashes)
			}
			//fmt.Printf("collect st write: %x, %016b\n", newK, branchSet)
			return storageIHCollector.Collect(newK, newV)
		}
		loader := trie.NewFlatDBTrieLoader(logPrefix)
		if err := loader.Reset(trie.NewRetainList(0), hashCollector /* HashCollector */, storageHashCollector, false); err != nil {
			return err
		}
		t := time.Now()
		hash, err := loader.CalcTrieRoot(db, []byte{}, quit)
		if err != nil {
			return err
		}
		generationIHTook := time.Since(t)
		if checkRoot && hash != expectedRootHash {
			return fmt.Errorf("%s: wrong trie root: %x, expected (from header): %x", logPrefix, hash, expectedRootHash)
		}
		log.Debug("Collection finished",
			"root hash", hash.Hex(),
			"gen IH", generationIHTook,
		)
		if err := accountIHCollector.Load(logPrefix, db,
			dbutils.IntermediateHashOfAccountBucket,
			etl.IdentityLoadFunc,
			etl.TransformArgs{
				Quit: quit,
			},
		); err != nil {
			return err
		}
		if err := storageIHCollector.Load(logPrefix, db,
			dbutils.IntermediateHashOfStorageBucket,
			etl.IdentityLoadFunc,
			etl.TransformArgs{
				Quit: quit,
			},
		); err != nil {
			return err
		}
	}
	log.Info(fmt.Sprintf("[%s] Regeneration ended", logPrefix))

	return nil
}

type HashPromoter struct {
	db               ethdb.Database
	ChangeSetBufSize uint64
	TempDir          string
	quitCh           <-chan struct{}
}

func NewHashPromoter(db ethdb.Database, quitCh <-chan struct{}) *HashPromoter {
	return &HashPromoter{
		db:               db,
		ChangeSetBufSize: 256 * 1024 * 1024,
		TempDir:          os.TempDir(),
		quitCh:           quitCh,
	}
}

func (p *HashPromoter) Promote(logPrefix string, s *StageState, from, to uint64, storage bool, load etl.LoadFunc, deleted map[string]struct{}) error {
	var changeSetBucket string
	if storage {
		changeSetBucket = dbutils.PlainStorageChangeSetBucket
	} else {
		changeSetBucket = dbutils.PlainAccountChangeSetBucket
	}
	log.Debug(fmt.Sprintf("[%s] Incremental state promotion of intermediate hashes", logPrefix), "from", from, "to", to, "csbucket", changeSetBucket)

	startkey := dbutils.EncodeBlockNumber(from + 1)

	decode := changeset.Mapper[changeSetBucket].Decode
	var extract etl.ExtractFunc
	var deletedAccounts map[string]struct{}
	if storage {
		extract = func(dbKey, dbValue []byte, next etl.ExtractNextFunc) error {
			_, k, _ := decode(dbKey, dbValue)
			newK, err := transformPlainStateKey(k)
			if err != nil {
				return err
			}

			return next(dbKey, newK, nil)
		}
	} else {
		deletedAccounts = map[string]struct{}{}
		extract = func(dbKey, dbValue []byte, next etl.ExtractNextFunc) error {
			_, k, v := decode(dbKey, dbValue)
			value, err := p.db.Get(dbutils.PlainStateBucket, k)
			if err != nil && !errors.Is(err, ethdb.ErrKeyNotFound) {
				return err
			}
			newK, err := transformPlainStateKey(k)
			if err != nil {
				return err
			}

			if len(value) == 0 && len(v) > 0 { // self-destructed
				newKS := string(newK)
				deletedAccounts[newKS] = struct{}{}
			}

			return next(dbKey, newK, nil)
		}
	}
	var l OldestAppearedLoad
	l.innerLoadFunc = load

	if err := etl.Transform(
		logPrefix,
		p.db,
		changeSetBucket,
		"",
		p.TempDir,
		extract,
		l.LoadFunc,
		etl.TransformArgs{
			BufferType:      etl.SortableOldestAppearedBuffer,
			ExtractStartKey: startkey,
			Quit:            p.quitCh,
		},
	); err != nil {
		return err
	}

	if !storage { // delete Intermediate hashes of deleted accounts
		for kS := range deletedAccounts {
			k := []byte(kS)
			if err := p.db.Walk(dbutils.IntermediateHashOfStorageBucket, k, 8*len(k), func(k, v []byte) (bool, error) {
				return true, p.db.Delete(dbutils.IntermediateHashOfStorageBucket, k, v)
			}); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func (p *HashPromoter) Unwind(logPrefix string, s *StageState, u *UnwindState, storage bool, load etl.LoadFunc) error {
	to := u.UnwindPoint
	var changeSetBucket string
	if storage {
		changeSetBucket = dbutils.PlainStorageChangeSetBucket
	} else {
		changeSetBucket = dbutils.PlainAccountChangeSetBucket
	}
	log.Info(fmt.Sprintf("[%s] Unwinding of intermediate hashes", logPrefix), "from", s.BlockNumber, "to", to, "csbucket", changeSetBucket)

	startkey := dbutils.EncodeBlockNumber(to + 1)

	decode := changeset.Mapper[changeSetBucket].Decode
	extract := func(dbKey, dbValue []byte, next etl.ExtractNextFunc) error {
		_, k, _ := decode(dbKey, dbValue)
		newK, err := transformPlainStateKey(k)
		if err != nil {
			return err
		}
		return next(k, newK, nil)
	}

	var l OldestAppearedLoad
	l.innerLoadFunc = load

	if err := etl.Transform(
		logPrefix,
		p.db,
		changeSetBucket,
		"",
		p.TempDir,
		extract,
		l.LoadFunc,
		etl.TransformArgs{
			BufferType:      etl.SortableOldestAppearedBuffer,
			ExtractStartKey: startkey,
			Quit:            p.quitCh,
		},
	); err != nil {
		return err
	}
	return nil
}

func incrementIntermediateHashes(logPrefix string, s *StageState, db ethdb.Database, to uint64, checkRoot bool, cache *shards.StateCache, tmpdir string, expectedRootHash common.Hash, quit <-chan struct{}) error {
	p := NewHashPromoter(db, quit)
	p.TempDir = tmpdir
	var exclude [][]byte
	collect := func(k []byte, v []byte, _ etl.CurrentTableReader, _ etl.LoadNextFunc) error {
		exclude = append(exclude, k)
		return nil
	}
	deletedAccounts := map[string]struct{}{}
	if err := p.Promote(logPrefix, s, s.BlockNumber, to, false /* storage */, collect, deletedAccounts); err != nil {
		return err
	}
	if err := p.Promote(logPrefix, s, s.BlockNumber, to, true /* storage */, collect, nil); err != nil {
		return err
	}

	defer func(t time.Time) { fmt.Printf("stage_interhashes.go:390: %s\n", time.Since(t)) }(time.Now())
	if cache != nil {
		var prefixes [16][][]byte
		for i := range exclude {
			id := exclude[i][0] / 16
			prefixes[id] = append(prefixes[id], exclude[i])
		}
		for i := range prefixes {
			prefix := prefixes[i]
			sort.Slice(prefix, func(i, j int) bool { return bytes.Compare(prefix[i], prefix[j]) < 0 })
			unfurl := trie.NewRetainList(0)
			for j := range prefix {
				unfurl.AddKey(prefix[j])
			}

			newV := make([]common.Hash, 0, 17)
			hashCollector := func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
				if len(keyHex) == 0 {
					return nil
				}
				if hashes == nil {
					cache.SetAccountHashDelete(keyHex)
					return nil
				}
				newV = newV[:len(hashes)/common.HashLength+len(rootHash)/common.HashLength]
				copyTo := newV
				if len(rootHash) > 0 {
					newV[0].SetBytes(rootHash)
					copyTo = newV[0:]
				}
				for j := 0; j < len(copyTo); j++ {
					copyTo[j].SetBytes(hashes[j*common.HashLength : (j+1)*common.HashLength])
				}
				cache.SetAccountHashWrite(keyHex, branchSet, set, newV)
				return nil
			}
			storageHashCollector := func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
				addr, inc := common.BytesToHash(accWithInc[:32]), binary.BigEndian.Uint64(accWithInc[32:])
				if hashes == nil {
					cache.SetStorageHashDelete(addr, inc, keyHex, branchSet, set, nil)
					return nil
				}
				newV = newV[:len(hashes)/common.HashLength+len(rootHash)/common.HashLength]
				copyTo := newV
				if len(rootHash) > 0 {
					newV[0].SetBytes(rootHash)
					copyTo = newV[0:]
				}
				for j := 0; j < len(copyTo); j++ {
					copyTo[j].SetBytes(hashes[j*common.HashLength : (j+1)*common.HashLength])
				}
				cache.SetStorageHashWrite(addr, inc, keyHex, branchSet, set, newV)
				return nil
			}
			// hashCollector in the line below will collect deletes
			loader := trie.NewFlatDBTrieLoader(logPrefix)
			if err := loader.Reset(unfurl, hashCollector, storageHashCollector, false); err != nil {
				return err
			}
			_, err := loader.CalcTrieRootOnCache(db, []byte{uint8(i)}, cache, quit)
			if err != nil {
				return err
			}
		}

		loader := trie.NewFlatDBTrieLoader(logPrefix)
		if err := loader.Reset(trie.NewRetainList(0), func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			return nil
		}, func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			return nil
		}, false); err != nil {
			return err
		}
		t := time.Now()
		hash, err := loader.CalcTrieRootOnCache2(cache)
		if err != nil {
			return err
		}
		generationIHTook := time.Since(t)
		if checkRoot && hash != expectedRootHash {
			return fmt.Errorf("%s: wrong trie root: %x, expected (from header): %x", logPrefix, hash, expectedRootHash)
		}
		log.Info(fmt.Sprintf("[%s] Collection finished", logPrefix),
			"root hash", hash.Hex(),
			"gen IH", generationIHTook,
		)

		writes := cache.PrepareWrites()
		shards.WalkAccountHashesWrites(writes, func(prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newV := make([]byte, len(h)*common.HashLength+4)
			binary.BigEndian.PutUint16(newV, branchChildren)
			binary.BigEndian.PutUint16(newV[2:], children)
			for i := 0; i < len(h); i++ {
				copy(newV[4+i*common.HashLength:4+(i+1)*common.HashLength], h[i].Bytes())
			}
			if err := db.Put(dbutils.IntermediateHashOfAccountBucket, prefix, newV); err != nil {
				panic(err)
			}
		}, func(prefix []byte, branchChildren, children uint16, h []common.Hash) {
			if err := db.Delete(dbutils.IntermediateHashOfAccountBucket, prefix, nil); err != nil {
				panic(err)
			}
		})
		shards.WalkStorageHashesWrites(writes, func(addrHash common.Hash, incarnation uint64, prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newV := make([]byte, len(h)*common.HashLength+4)
			binary.BigEndian.PutUint16(newV, branchChildren)
			binary.BigEndian.PutUint16(newV[2:], children)
			for i := 0; i < len(h); i++ {
				copy(newV[4+i*common.HashLength:4+(i+1)*common.HashLength], h[i].Bytes())
			}
			newK := dbutils.GenerateCompositeStoragePrefix(addrHash.Bytes(), incarnation, prefix)
			if err := db.Put(dbutils.IntermediateHashOfStorageBucket, newK, newV); err != nil {
				panic(err)
			}
		}, func(addrHash common.Hash, incarnation uint64, prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newK := dbutils.GenerateCompositeStoragePrefix(addrHash.Bytes(), incarnation, prefix)
			if err := db.Delete(dbutils.IntermediateHashOfStorageBucket, newK, nil); err != nil {
				panic(err)
			}
		})
		cache.TurnWritesToReads(writes)
	} else {
		sort.Slice(exclude, func(i, j int) bool { return bytes.Compare(exclude[i], exclude[j]) < 0 })
		unfurl := trie.NewRetainList(0)
		//fmt.Printf("excl: %d\n", len(exclude))
		for i := range exclude {
			//fmt.Printf("excl: %x\n", exclude[i])
			unfurl.AddKey(exclude[i])
		}

		accountIHCollector := etl.NewCollector(tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
		storageIHCollector := etl.NewCollector(tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
		newV := make([]byte, 0, 1024)
		hashCollector := func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			if len(keyHex) == 0 {
				return nil
			}
			if hashes == nil {
				//fmt.Printf("collect del: %x\n", keyHex)
				return accountIHCollector.Collect(keyHex, nil)
			}
			newV = newV[:len(hashes)+len(rootHash)+4]
			binary.BigEndian.PutUint16(newV, branchSet)
			binary.BigEndian.PutUint16(newV[2:], set)
			if len(rootHash) == 0 {
				copy(newV[4:], hashes)
			} else {
				copy(newV[4:], rootHash)
				copy(newV[36:], hashes)
			}
			//fmt.Printf("collect write: %x, %016b\n", keyHex, branchSet)
			return accountIHCollector.Collect(keyHex, newV)
		}
		newK := make([]byte, 0, 128)
		storageHashCollector := func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			newK = append(append(newK[:0], accWithInc...), keyHex...)
			if hashes == nil {
				return storageIHCollector.Collect(newK, nil)
			}
			newV = newV[:len(hashes)+len(rootHash)+4]
			binary.BigEndian.PutUint16(newV, branchSet)
			binary.BigEndian.PutUint16(newV[2:], set)
			if len(rootHash) == 0 {
				copy(newV[4:], hashes)
			} else {
				copy(newV[4:], rootHash)
				copy(newV[36:], hashes)
			}
			return storageIHCollector.Collect(newK, newV)
		}
		// hashCollector in the line below will collect deletes
		loader := trie.NewFlatDBTrieLoader(logPrefix)
		if err := loader.Reset(unfurl, hashCollector, storageHashCollector, false); err != nil {
			return err
		}
		t := time.Now()
		hash, err := loader.CalcTrieRoot(db, []byte{}, quit)
		if err != nil {
			return err
		}
		generationIHTook := time.Since(t)
		if checkRoot && hash != expectedRootHash {
			return fmt.Errorf("%s: wrong trie root: %x, expected (from header): %x", logPrefix, hash, expectedRootHash)
		}
		log.Info(fmt.Sprintf("[%s] Collection finished", logPrefix),
			"root hash", hash.Hex(),
			"gen IH", generationIHTook,
		)
		if err := accountIHCollector.Load(logPrefix, db,
			dbutils.IntermediateHashOfAccountBucket,
			etl.IdentityLoadFunc,
			etl.TransformArgs{
				Quit: quit,
			},
		); err != nil {
			return err
		}
		if err := storageIHCollector.Load(logPrefix, db,
			dbutils.IntermediateHashOfStorageBucket,
			etl.IdentityLoadFunc,
			etl.TransformArgs{
				Quit: quit,
			},
		); err != nil {
			return err
		}
	}
	return nil
}

func UnwindIntermediateHashesStage(u *UnwindState, s *StageState, db ethdb.Database, cache *shards.StateCache, tmpdir string, quit <-chan struct{}) error {
	hash, err := rawdb.ReadCanonicalHash(db, u.UnwindPoint)
	if err != nil {
		return fmt.Errorf("read canonical hash: %w", err)
	}
	syncHeadHeader := rawdb.ReadHeader(db, hash, u.UnwindPoint)
	expectedRootHash := syncHeadHeader.Root
	fmt.Printf("u: %d->%d\n", s.BlockNumber, u.UnwindPoint)

	var tx ethdb.DbWithPendingMutations
	var useExternalTx bool
	if hasTx, ok := db.(ethdb.HasTx); ok && hasTx.Tx() != nil {
		tx = db.(ethdb.DbWithPendingMutations)
		useExternalTx = true
	} else {
		var err error
		tx, err = db.Begin(context.Background(), ethdb.RW)
		if err != nil {
			return fmt.Errorf("open transcation: %w", err)
		}
		defer tx.Rollback()
	}

	logPrefix := s.state.LogPrefix()
	if err := unwindIntermediateHashesStageImpl(logPrefix, u, s, tx, cache, tmpdir, expectedRootHash, quit); err != nil {
		return err
	}
	if err := u.Done(tx); err != nil {
		return fmt.Errorf("%s: reset: %w", logPrefix, err)
	}
	if !useExternalTx {
		if _, err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func unwindIntermediateHashesStageImpl(logPrefix string, u *UnwindState, s *StageState, db ethdb.Database, cache *shards.StateCache, tmpdir string, expectedRootHash common.Hash, quit <-chan struct{}) error {
	p := NewHashPromoter(db, quit)
	p.TempDir = tmpdir
	var exclude [][]byte
	collect := func(k []byte, _ []byte, _ etl.CurrentTableReader, _ etl.LoadNextFunc) error {
		exclude = append(exclude, k)
		return nil
	}
	if err := p.Unwind(logPrefix, s, u, false /* storage */, collect); err != nil {
		return err
	}
	if err := p.Unwind(logPrefix, s, u, true /* storage */, collect); err != nil {
		return err
	}

	if cache != nil {
		var prefixes [16][][]byte
		for i := range exclude {
			id := exclude[i][0] / 16
			prefixes[id] = append(prefixes[id], exclude[i])
		}
		for i := range prefixes {
			prefix := prefixes[i]
			sort.Slice(prefix, func(i, j int) bool { return bytes.Compare(prefix[i], prefix[j]) < 0 })
			unfurl := trie.NewRetainList(0)
			for i := range prefix {
				unfurl.AddKey(prefix[i])
			}

			newV := make([]common.Hash, 0, 17)
			hashCollector := func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
				if len(keyHex) == 0 {
					return nil
				}
				if hashes == nil {
					cache.SetAccountHashDelete(keyHex)
					return nil
				}
				newV = newV[:len(hashes)/common.HashLength+len(rootHash)/common.HashLength]
				copyTo := newV
				if len(rootHash) > 0 {
					newV[0].SetBytes(rootHash)
					copyTo = newV[0:]
				}
				for j := 0; j < len(copyTo); j++ {
					copyTo[j].SetBytes(hashes[j*common.HashLength : (j+1)*common.HashLength])
				}
				cache.SetAccountHashWrite(keyHex, branchSet, set, newV)
				return nil
			}
			storageHashCollector := func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
				addr, inc := common.BytesToHash(accWithInc[:32]), binary.BigEndian.Uint64(accWithInc[32:])
				if hashes == nil {
					cache.SetStorageHashDelete(addr, inc, keyHex, branchSet, set, nil)
					return nil
				}
				newV = newV[:len(hashes)/common.HashLength+len(rootHash)/common.HashLength]
				copyTo := newV
				if len(rootHash) > 0 {
					newV[0].SetBytes(rootHash)
					copyTo = newV[0:]
				}
				for j := 0; j < len(copyTo); j++ {
					copyTo[j].SetBytes(hashes[j*common.HashLength : (j+1)*common.HashLength])
				}
				cache.SetStorageHashWrite(addr, inc, keyHex, branchSet, set, newV)
				return nil
			}
			loader := trie.NewFlatDBTrieLoader(logPrefix)
			// hashCollector in the line below will collect deletes
			if err := loader.Reset(unfurl, hashCollector, storageHashCollector, false); err != nil {
				return err
			}
			_, err := loader.CalcTrieRootOnCache(db, []byte{uint8(i)}, cache, quit)
			if err != nil {
				return err
			}
		}

		loader := trie.NewFlatDBTrieLoader(logPrefix)
		if err := loader.Reset(trie.NewRetainList(0), func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			return nil
		}, func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			return nil
		}, false); err != nil {
			return err
		}
		t := time.Now()
		hash, err := loader.CalcTrieRootOnCache2(cache)
		if err != nil {
			return err
		}
		generationIHTook := time.Since(t)
		if hash != expectedRootHash {
			return fmt.Errorf("%s: wrong trie root: %x, expected (from header): %x", logPrefix, hash, expectedRootHash)
		}
		log.Info(fmt.Sprintf("[%s] Collection finished", logPrefix),
			"root hash", hash.Hex(),
			"gen IH", generationIHTook,
		)

		writes := cache.PrepareWrites()
		shards.WalkAccountHashesWrites(writes, func(prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newV := make([]byte, len(h)*common.HashLength+4)
			binary.BigEndian.PutUint16(newV, branchChildren)
			binary.BigEndian.PutUint16(newV[2:], children)
			for i := 0; i < len(h); i++ {
				copy(newV[4+i*common.HashLength:4+(i+1)*common.HashLength], h[i].Bytes())
			}
			if err := db.Put(dbutils.IntermediateHashOfAccountBucket, prefix, newV); err != nil {
				panic(err)
			}
		}, func(prefix []byte, branchChildren, children uint16, h []common.Hash) {
			if err := db.Delete(dbutils.IntermediateHashOfAccountBucket, prefix, nil); err != nil {
				panic(err)
			}
		})
		shards.WalkStorageHashesWrites(writes, func(addrHash common.Hash, incarnation uint64, prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newV := make([]byte, len(h)*common.HashLength+4)
			binary.BigEndian.PutUint16(newV, branchChildren)
			binary.BigEndian.PutUint16(newV[2:], children)
			for i := 0; i < len(h); i++ {
				copy(newV[4+i*common.HashLength:4+(i+1)*common.HashLength], h[i].Bytes())
			}
			newK := dbutils.GenerateCompositeStoragePrefix(addrHash.Bytes(), incarnation, prefix)
			if err := db.Put(dbutils.IntermediateHashOfStorageBucket, newK, newV); err != nil {
				panic(err)
			}
		}, func(addrHash common.Hash, incarnation uint64, prefix []byte, branchChildren, children uint16, h []common.Hash) {
			newK := dbutils.GenerateCompositeStoragePrefix(addrHash.Bytes(), incarnation, prefix)
			if err := db.Delete(dbutils.IntermediateHashOfStorageBucket, newK, nil); err != nil {
				panic(err)
			}
		})
		cache.TurnWritesToReads(writes)

	} else {
		sort.Slice(exclude, func(i, j int) bool { return bytes.Compare(exclude[i], exclude[j]) < 0 })
		unfurl := trie.NewRetainList(0)
		//fmt.Printf("excl: %d\n", len(exclude))
		for i := range exclude {
			//fmt.Printf("excl: %x\n", exclude[i])
			unfurl.AddKey(exclude[i])
		}

		accountIHCollector := etl.NewCollector(tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
		storageIHCollector := etl.NewCollector(tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
		newV := make([]byte, 0, 1024)
		hashCollector := func(keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			if len(keyHex) == 0 {
				return nil
			}
			if hashes == nil {
				//fmt.Printf("collect del: %x\n", keyHex)
				return accountIHCollector.Collect(keyHex, nil)
			}
			newV = newV[:len(hashes)+len(rootHash)+4]
			binary.BigEndian.PutUint16(newV, branchSet)
			binary.BigEndian.PutUint16(newV[2:], set)
			if len(rootHash) == 0 {
				copy(newV[4:], hashes)
			} else {
				copy(newV[4:], rootHash)
				copy(newV[36:], hashes)
			}
			//fmt.Printf("collect write: %x, %016b\n", keyHex, branchSet)
			return accountIHCollector.Collect(keyHex, newV)
		}
		newK := make([]byte, 0, 128)
		storageHashCollector := func(accWithInc []byte, keyHex []byte, set uint16, branchSet uint16, hashes []byte, rootHash []byte) error {
			newK = append(append(newK[:0], accWithInc...), keyHex...)
			if hashes == nil {
				return storageIHCollector.Collect(newK, nil)
			}
			newV = newV[:len(hashes)+len(rootHash)+4]
			binary.BigEndian.PutUint16(newV, branchSet)
			binary.BigEndian.PutUint16(newV[2:], set)
			if len(rootHash) == 0 {
				copy(newV[4:], hashes)
			} else {
				copy(newV[4:], rootHash)
				copy(newV[36:], hashes)
			}
			return storageIHCollector.Collect(newK, newV)
		}
		// hashCollector in the line below will collect deletes
		loader := trie.NewFlatDBTrieLoader(logPrefix)
		if err := loader.Reset(unfurl, hashCollector, storageHashCollector, false); err != nil {
			return err
		}
		t := time.Now()
		hash, err := loader.CalcTrieRoot(db, []byte{}, quit)
		if err != nil {
			return err
		}
		generationIHTook := time.Since(t)
		if hash != expectedRootHash {
			return fmt.Errorf("%s: wrong trie root: %x, expected (from header): %x", logPrefix, hash, expectedRootHash)
		}
		log.Info(fmt.Sprintf("[%s] Collection finished", logPrefix),
			"root hash", hash.Hex(),
			"gen IH", generationIHTook,
		)
		if err := accountIHCollector.Load(logPrefix, db,
			dbutils.IntermediateHashOfAccountBucket,
			etl.IdentityLoadFunc,
			etl.TransformArgs{
				Quit: quit,
			},
		); err != nil {
			return err
		}
		if err := storageIHCollector.Load(logPrefix, db,
			dbutils.IntermediateHashOfStorageBucket,
			etl.IdentityLoadFunc,
			etl.TransformArgs{
				Quit: quit,
			},
		); err != nil {
			return err
		}
	}
	return nil
}

func ResetHashState(db ethdb.Database) error {
	if err := db.(ethdb.BucketsMigrator).ClearBuckets(
		dbutils.HashedAccountsBucket,
		dbutils.HashedStorageBucket,
		dbutils.ContractCodeBucket,
	); err != nil {
		return err
	}
	batch := db.NewBatch()
	if err := stages.SaveStageProgress(batch, stages.HashState, 0); err != nil {
		return err
	}
	if err := stages.SaveStageUnwind(batch, stages.HashState, 0); err != nil {
		return err
	}
	if _, err := batch.Commit(); err != nil {
		return err
	}

	return nil
}

func ResetIH(db ethdb.Database) error {
	if err := db.(ethdb.BucketsMigrator).ClearBuckets(
		dbutils.IntermediateHashOfAccountBucket,
		dbutils.IntermediateHashOfStorageBucket,
	); err != nil {
		return err
	}
	batch := db.NewBatch()
	if err := stages.SaveStageProgress(batch, stages.IntermediateHashes, 0); err != nil {
		return err
	}
	if err := stages.SaveStageUnwind(batch, stages.IntermediateHashes, 0); err != nil {
		return err
	}
	if _, err := batch.Commit(); err != nil {
		return err
	}

	return nil
}
