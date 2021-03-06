package bubt

import "fmt"
import "encoding/binary"

import "github.com/bnclabs/gostore/lib"

var _ = fmt.Sprintf("")

type zblock struct {
	zblocksize int64
	vblocksize int64
	firstkey   []byte
	index      blkindex
	vlog       []byte // value buffer will be valid if vblocksize is > 0
	vlogpos    int64
	buffer     []byte

	// working buffer
	zerovbuff []byte
	entries   []byte // points into buffer
	block     []byte // points into buffer
}

// zblock represents the leaf node in bubt tree, can be in a separate
// file, shape of block is:
//
// n_entries uint32   - 4-byte count of number entries in this zblock.
// blkindex  []uint32 - 4 byte offset into zblock for each entry.
// zentries           - array of zentries.
func newz(zblocksize, vblocksize int64) (z *zblock) {
	z = &zblock{
		zblocksize: zblocksize,
		vblocksize: vblocksize,
		firstkey:   make([]byte, 0, 256),
		index:      make(blkindex, 0, 64),
		buffer:     make([]byte, zblocksize*2),
	}
	if z.vblocksize > 0 {
		z.zerovbuff = make([]byte, vblocksize)
	}
	z.entries = z.buffer[zblocksize:zblocksize]
	return
}

func (z *zblock) reset(vlogpos int64, vlog []byte) *zblock {
	z.firstkey = z.firstkey[:0]
	z.index = z.index[:0]
	z.vlog, z.vlogpos = vlog, vlogpos
	z.buffer = z.buffer[:z.zblocksize*2]
	z.entries = z.entries[:0]
	z.block = nil
	return z
}

func (z *zblock) insert(
	key, value []byte, valuelen uint64, vlogpos int64,
	seqno uint64, deleted bool) bool {

	//fmt.Println(len(key), len(value), z.zblocksize)
	if key == nil {
		return false
	} else if z.isoverflow(key, value, deleted) {
		return false
	}

	z.index = append(z.index, uint32(len(z.entries)))

	var scratch [24]byte
	ze := zentry(scratch[:])
	ze = ze.setseqno(seqno).setkeylen(uint64(len(key)))

	if deleted {
		ze.setdeleted().setvaluelen(0)
		z.entries = append(z.entries, scratch[:]...)
		z.entries = append(z.entries, key...)

	} else if len(value) == 0 && vlogpos < 0 { // no value
		ze.cleardeleted().setvaluelen(0)
		z.entries = append(z.entries, scratch[:]...)
		z.entries = append(z.entries, key...)

	} else if len(value) == 0 { // value-ref to value-log
		ze.setvlog().cleardeleted().setvaluelen(valuelen)
		z.entries = append(z.entries, scratch[:]...)
		z.entries = append(z.entries, key...)
		binary.BigEndian.PutUint64(scratch[:8], uint64(vlogpos))
		z.entries = append(z.entries, scratch[:8]...)

	} else {
		var ok bool
		var vle vlogentry
		var vlogpos int64

		valuelen = uint64(len(value))
		ok, vlogpos, z.vlogpos, z.vlog = vle.serialize(
			z.vblocksize, z.vlogpos, value, z.vlog, z.zerovbuff,
		)
		if ok { // value in vlog file
			ze.setvlog()
		}
		ze.cleardeleted().setvaluelen(valuelen)
		z.entries = append(z.entries, scratch[:]...)
		z.entries = append(z.entries, key...)
		if ok == false { // value in zblock.
			z.entries = append(z.entries, value...)
		} else if vlogpos > 0 { // value in vlog
			binary.BigEndian.PutUint64(scratch[:8], uint64(vlogpos))
			z.entries = append(z.entries, scratch[:8]...)
		}
	}

	z.setfirstkey(key)

	return true
}

func (z *zblock) finalize() (int64, bool) {
	if len(z.index) == 0 {
		return 0, false
	}
	indexlen := z.index.footprint()
	block := z.buffer[z.zblocksize-indexlen : int64(len(z.buffer))-indexlen]
	// 4-byte length of index array.
	binary.BigEndian.PutUint32(block, uint32(z.index.length()))
	// each index entry is 4 byte, index point into z-block for zentry.
	n := 4
	for _, entryoff := range z.index {
		binary.BigEndian.PutUint32(block[n:], uint32(indexlen)+entryoff)
		n += 4
	}
	// ZERO padding
	n += len(z.entries)
	padded := len(block[n:])
	for i := range block[n:] {
		block[n+i] = 0
	}
	z.block = block
	return int64(padded), true
}

//---- local methods

func (z *zblock) isoverflow(key, value []byte, deleted bool) bool {
	entrysz := int64(zentrysize + len(key))
	if deleted == false {
		if z.vblocksize > 0 {
			entrysz += 8 // just file position into value log.
		} else {
			entrysz += int64(len(value))
		}
	}
	total := int64(len(z.entries)) + entrysz + z.index.nextfootprint()
	if total > z.zblocksize {
		return true
	}
	return false
}

func (z *zblock) setfirstkey(key []byte) {
	if len(z.firstkey) == 0 {
		z.firstkey = lib.Fixbuffer(z.firstkey, int64(len(key)))
		copy(z.firstkey, key)
	}
}
