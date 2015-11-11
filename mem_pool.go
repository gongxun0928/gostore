package llrb

//#include <stdlib.h>
import "C"

import "unsafe"
import "encoding/binary"

// mempool manages a memory block sliced up into equal sized chunks.
type mempool struct {
	capacity   int            // memory managed by this pool
	size       int            // fixed size blocks in this pool
	base       unsafe.Pointer // pool's base pointer
	freelist   []uint8        // free block book-keeping
	freeoff    int
	mallocated int
}

func newmempool(size, n int) *mempool {
	if (n & 0x7) != 0 {
		panic("number of blocks in a pool should be multiple of 8")
	}
	capacity := size * n
	freelist := make([]uint8, n/8)
	for i := range freelist {
		freelist[i] = 0xff // every block is free to begin with.
	}
	pool := &mempool{
		capacity: capacity,
		size:     size,
		base:     C.malloc(C.size_t(capacity)),
		freelist: freelist,
		freeoff:  0,
	}
	return pool
}

func (pool *mempool) alloc() (unsafe.Pointer, bool) {
	var safeoff int

	if pool.freeoff < 0 {
		return nil, false
	}
	byt := pool.freelist[pool.freeoff]
	if byt == 0 {
		panic("mempool.alloc(): invalid free-offset")
	}
	sz, k := pool.size, findfirstset8(byt)
	ptr := uintptr(pool.base) + uintptr(((pool.freeoff*8)*sz)+(int(k)*sz))
	pool.freelist[pool.freeoff] = clearbit8(byt, uint8(k))
	// recompute freeoff
	safeoff, pool.freeoff = pool.freeoff, -1
	for i := safeoff; i < len(pool.freelist); i++ {
		if pool.freelist[i] != 0 {
			pool.freeoff = i
			break
		}
	}
	pool.mallocated += pool.size
	return unsafe.Pointer(ptr), true
}

func (pool *mempool) free(ptr unsafe.Pointer) {
	if ptr == nil {
		panic("mempool.free(): nil pointer")
	}
	diffptr := uint64(uintptr(ptr) - uintptr(pool.base))
	if (diffptr % uint64(pool.size)) != 0 {
		panic("mempool.free(): unaligned pointer")
	}
	nthblock := diffptr / uint64(pool.size)
	nthoff := (nthblock / 8)
	pool.freelist[nthoff] = setbit8(pool.freelist[nthoff], uint8(nthblock%8))
	if pool.freeoff == -1 || int(nthoff) < pool.freeoff {
		pool.freeoff = int(nthoff)
	}
	pool.mallocated -= pool.size
}

func (pool *mempool) release() {
	C.free(pool.base)
	pool.freelist, pool.freeoff = nil, -1
	pool.capacity, pool.base = 0, nil
}

// compare whether pool's base ptr is less than other pool's base ptr.
func (pool *mempool) less(other *mempool) bool {
	return uintptr(pool.base) < uintptr(other.base)
}

//---- local functions

func (pool *mempool) memory() int {
	self := int(unsafe.Sizeof(*pool))
	slicesz := cap(pool.freelist)
	return pool.capacity + slicesz + self
}

func (pool *mempool) allocated() int {
	return pool.mallocated
}

func (pool *mempool) allocated1() int {
	blocks := 0
	q, r := len(pool.freelist)/4, len(pool.freelist)%4
	for i := 1; i <= q; i++ {
		v := binary.BigEndian.Uint32(pool.freelist[(i-1)*4 : i*4])
		blocks += int(zerosin32(uint32(v & 0xffffffff)))
	}
	for i := q * 4; i < (q*4)+r; i++ {
		blocks += int(zerosin8(pool.freelist[i]))
	}
	return blocks * pool.size
}

func (pool *mempool) available() int {
	return pool.capacity - pool.allocated()
}

// mempools sortable based on base-pointer.
type mempools []*mempool

func (pools mempools) Len() int {
	return len(pools)
}

func (pools mempools) Less(i, j int) bool {
	return pools[i].less(pools[j])
}

func (pools mempools) Swap(i, j int) {
	pools[i], pools[j] = pools[j], pools[i]
}