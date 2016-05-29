// +build ignore

package storage

import "encoding/binary"
import "fmt"

type bubtzblock struct {
	f        *bubtstore
	fpos     [2]int64 // kpos, vpos
	rpos     int64
	firstkey []byte
	entries  []uint32
	kbuffer  []byte
	dbuffer  []byte
}

func (f *bubtstore) newz(fpos [2]int64) (z *bubtzblock) {
	select {
	case z = f.zpool:
		z.f, z.fpos = f, fpos
		z.firstkey = z.firstkey[:0]
		z.entries = z.entries[:0]
		z.kbuffer, z.dbuffer = f.getbuffer(), f.getbuffer()

	default:
		z = &bubtzblock{
			f:        f,
			fpos:     fpos,
			firstkey: make([]byte, 0, MaxKeymem),
			entries:  make([]uint32, 0),
		}
		z.kbuffer, z.dbuffer = f.getbuffer(), f.getbuffer()
	}
	f.znodes++
	return
}

func (z *bubtzblock) insert(nd Node) (ok bool) {
	var key, value []byte
	var scratch [26]byte

	if nd == nil {
		return false
	} else if key, value = nd.Key(), nd.Value(); len(key) > MaxKeymem {
		panic(fmt.Errorf("key cannot exceed %v", MaxKeymem))
	} else if len(value) > MaxValmem {
		panic(fmt.Errorf("value cannot exceed %v", MaxValmem))
	}

	// check whether enough space available in the block.
	entrysz := len(scratch) + 2 + len(key) // TODO: avoid magic numbers
	if z.f.hasdatafile() {
		entrysz += 8
	} else {
		entrysz += 2 + len(value) // TODO: avoid magic numbers
	}
	arrayblock := 4 + (len(z.entries) * 4)
	if (arrayblock + len(z.kbuffer) + entrysz) > z.f.zblocksize {
		return false
	}

	if len(z.firstkey) == 0 {
		z.firstkey = z.firstkey[:len(key)]
		copy(z.firstkey, key)
	}

	z.entries = append(z.entries, len(z.kbuffer))
	z.f.a_keysize.add(int64(len(key)))
	z.f.a_valsize.add(int64(len(value)))

	// encode metadadata {vbno(2), vbuuid(8), bornseqno(8), deadseqno(8)}
	binary.BigEndian.PutUint16(scratch[:2], nd.Vbno())         // 2 bytes
	binary.BigEndian.PutUint64(scratch[2:10], nd.Vbuuid())     // 8 bytes
	binary.BigEndian.PutUint64(scratch[10:18], nd.Bornseqno()) // 8 bytes
	binary.BigEndian.PutUint64(scratch[18:26], nd.Deadseqno()) // 8 bytes
	z.kbuffer = append(z.kbuffer, scratch[:26]...)
	// encode key {keylen(2-byte), key(n-byte)}
	binary.BigEndian.PutUint16(scratch[:2], len(key))
	z.kbuffer = append(z.kbuffer, scratch[:2]...)
	z.kbuffer = append(z.kbuffer, key...)
	// encode value
	if z.f.hasdatafile() {
		vpos := z.fpos[1] + len(z.dbuffer)
		binary.BigEndian.PutUint16(scratch[:2], len(value))
		z.dbuffer = append(z.dbuffer, scratch[:2]...)
		z.dbuffer = append(z.dbuffer, value...)
		binary.BigEndian.PutUint64(scratch[:8], vpos)
		z.kbuffer = append(z.kbuffer, scratch[:8]...)
	} else {
		binary.BigEndian.PutUint16(scratch[:2], len(value))
		z.kbuffer = append(z.kbuffer, scratch[:2]...)
		z.kbuffer = append(z.kbuffer, value...)
	}
	return true
}

func (z *bubtzblock) startkey() (int64, []byte) {
	if len(z.entries) > 0 {
		koff := binary.BigEndian.Uint32(z.kbuffer[4:8])
		return z.fpos[0] + koff, z.firstkey
	}
	return z.fpos[0], nil
}

func (z *bubtzblock) offset() int64 {
	return z.fpos[0]
}

func (z *bubtzblock) roffset() int64 {
	return z.rpos
}

func (z *bubtzblock) finalize() {
	arrayblock := 4 + (len(z.entries) * 4)
	sz, ln := arrayblock+len(z.kbuffer), len(z.kbuffer)
	if sz > z.f.zblocksize {
		fmsg := "zblock buffer overflow %v > %v, call the programmer!"
		panic(fmt.Sprintf(fmsg, sz, z.f.zblocksize))
	}

	z.kbuffer = z.kbuffer[:sz] // first increase slice length

	copy(z.kbuffer[arrayblock:], z.kbuffer[:ln])
	n := 0
	binary.BigEndian.PutUint32(z.kbuffer[n:], uint32(len(z.entries)))
	n += 4
	for _, koff := range z.entries {
		binary.BigEndian.PutUint32(z.kbuffer[n:], arrayblock+koff)
		n += 4
	}
}

func (z *bubtzblock) reduce() []byte {
	if z.f.mreduce {
		if z.f.hasdatafile() {
			return nil
		}
		panic("enable datafile for mreduce")
	}
	return nil
}
