package bitmapdb

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/RoaringBitmap/roaring"
	"github.com/c2h5oh/datasize"
	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/ethdb"
)

const ColdShardLimit = 512 * datasize.KB
const HotShardLimit = 4 * datasize.KB

// AppendMergeByOr - appending delta to existing data in db, merge by Or
// Method maintains sharding - because some bitmaps are >1Mb and when new incoming blocks process it
//	 updates ~300 of bitmaps - by append small amount new values. It cause much big writes (LMDB does copy-on-write).
//
// 3 terms are used: cold_shard, hot_shard, delta
//   delta - most recent changes (appendable)
//   hot_shard - merge delta here until hot_shard size < HotShardLimit, otherwise merge hot to cold
//   cold_shard - merge hot_shard here until cold_shard size < ColdShardLimit, otherwise mark hot as cold, create new hot from delta
// if no cold shard, create new hot from delta - it will turn hot to cold automatically
// never merges cold with cold for compaction - because it's expensive operation
func AppendMergeByOr(c ethdb.Cursor, key []byte, delta *roaring.Bitmap) error {
	shardNForDelta := uint16(0)

	hotK, hotV, seekErr := c.Seek(key)
	if seekErr != nil {
		return seekErr
	}
	if hotK != nil && bytes.HasPrefix(hotK, key) {
		hotShardN := ^binary.BigEndian.Uint16(hotK[len(hotK)-2:])
		if len(hotV) > int(HotShardLimit) { // merge hot to cold
			var err error
			shardNForDelta, err = hotShardOverflow(c, key, hotShardN, hotV)
			if err != nil {
				return err
			}
		} else { // merge delta to hot, write result to `hotShardN`
			hot := roaring.New()
			_, err := hot.FromBuffer(hotV)
			if err != nil {
				return err
			}

			delta = roaring.FastOr(delta, hot)
			shardNForDelta = hotShardN
		}
	}

	newK := make([]byte, len(key)+2)
	copy(newK, key)
	binary.BigEndian.PutUint16(newK[len(newK)-2:], ^shardNForDelta)

	//delta.RunOptimize()
	buf := bytes.NewBuffer(make([]byte, 0, delta.GetSerializedSizeInBytes()))
	_, err := delta.WriteTo(buf)
	if err != nil {
		return err
	}
	err = c.Put(newK, buf.Bytes())
	if err != nil {
		return err
	}
	return nil
}

func hotShardOverflow(c ethdb.Cursor, initialKey []byte, hotShardN uint16, hotV []byte) (shardNForDelta uint16, err error) {
	if hotShardN == 0 { // no cold shards, create new hot from delta - it will turn hot to cold automatically
		return 1, nil
	}

	coldK, coldV, err := c.Next() // get cold shard from db
	if err != nil {
		return 0, err
	}

	if coldK == nil || !bytes.HasPrefix(coldK, initialKey) {
		return 0, fmt.Errorf("cold shard not found in db: key=%x, hotShardN=%d, found=%x", initialKey, hotShardN, coldK)
	}

	if len(coldV) > int(ColdShardLimit) { // cold shard is too big, write delta to `hotShardN + 1` - it will turn hot to cold automatically
		return hotShardN + 1, nil
	}

	// merge hot to cold and replace hot by delta (by write delta to `hotShardN`)
	cold := roaring.New()
	_, err = cold.FromBuffer(coldV)
	if err != nil {
		return 0, err
	}

	hot := roaring.New()
	_, err = hot.FromBuffer(hotV)
	if err != nil {
		return 0, err
	}

	cold = roaring.FastOr(cold, hot)

	//cold.RunOptimizecold
	buf := bytes.NewBuffer(make([]byte, 0, cold.GetSerializedSizeInBytes()))
	_, err = cold.WriteTo(buf)
	if err != nil {
		return 0, err
	}
	err = c.Put(common.CopyBytes(coldK), buf.Bytes()) // copy 'coldK' if want replace c.PutCurrent() by c.Put()
	if err != nil {
		return 0, err
	}

	return hotShardN, nil
}

// TruncateRange - gets existing bitmap in db and call RemoveRange operator on it.
// starts from hot shard, stops when shard not overlap with [from-to)
// !Important: [from, to)
func TruncateRange(c ethdb.Cursor, key []byte, from, to uint64) error {
	updated := 0
	for k, v, err := c.Seek(key); k != nil; k, v, err = c.Next() {
		if err != nil {
			return err
		}

		if !bytes.HasPrefix(k, key) {
			break
		}

		bm := roaring.New()
		_, err = bm.FromBuffer(v)
		if err != nil {
			return err
		}
		if uint64(bm.Maximum()) < from {
			break
		}
		noReasonToCheckNextShard := uint64(bm.Minimum()) <= from && uint64(bm.Maximum()) >= to

		updated++
		bm.RemoveRange(from, to)
		if bm.GetCardinality() == 0 { // don't store empty bitmaps
			err = c.DeleteCurrent()
			if err != nil {
				return err
			}
			continue
		}

		bm.RunOptimize()

		buf := bytes.NewBuffer(make([]byte, 0, bm.GetSerializedSizeInBytes()))
		_, err = bm.WriteTo(buf)
		if err != nil {
			return err
		}
		err = c.Put(common.CopyBytes(k), buf.Bytes())
		if err != nil {
			return err
		}

		if noReasonToCheckNextShard {
			break
		}
	}

	return nil
}

func Get(c ethdb.Cursor, key []byte) (*roaring.Bitmap, error) {
	var shards []*roaring.Bitmap
	for k, v, err := c.Seek(key); k != nil; k, v, err = c.Next() {
		if err != nil {
			return nil, err
		}

		if !bytes.HasPrefix(k, key) {
			break
		}

		bm := roaring.New()
		_, err = bm.FromBuffer(v)
		if err != nil {
			return nil, err
		}
		shards = append(shards, bm)
	}

	if len(shards) == 0 {
		return roaring.New(), nil
	}
	return roaring.FastOr(shards...), nil
}
