package llrb

import "testing"
import "bytes"
import "fmt"

var _ = fmt.Sprintf("dummy")

func TestNodeValue(t *testing.T) {
	minblock, maxblock, capacity := 96, 1024*1024*10, 1024*1024*1024
	marena := newmemarena(minblock, maxblock, capacity)

	blocksize, value := 1024, []byte("hello world")
	ptr, mpool := marena.alloc(blocksize)
	nv := (*nodevalue)(ptr)
	nv = nv.setvalsize(len(value)).setvalue(value)
	if x := nv.valsize(); x != len(value) {
		t.Errorf("expected %v, got %v", len(value), x)
	} else if v := nv.value(); bytes.Compare(value, v) != 0 {
		t.Errorf("expected %v, got %v", value, v)
	}
	mpool.free(ptr)
}

func BenchmarkValueSize(b *testing.B) {
	minblock, maxblock, capacity := 96, 1024*1024*10, 1024*1024*1024
	blocksize, value := 1024, []byte("hello world")

	marena := newmemarena(minblock, maxblock, capacity)
	ptr, mpool := marena.alloc(blocksize)
	nv := (*nodevalue)(ptr)
	nv.pool = mpool
	nv = nv.setvalsize(len(value)).setvalue(value)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		nv.setvalsize(1024)
		nv.valsize()
	}
	mpool.free(ptr)
}

func BenchmarkSetValue(b *testing.B) {
	minblock, maxblock, capacity := 96, 1024*1024*10, 1024*1024*1024
	blocksize, value := 20*1024, make([]byte, 10*1024)

	marena := newmemarena(minblock, maxblock, capacity)
	ptr, mpool := marena.alloc(blocksize)
	nv := (*nodevalue)(ptr)
	nv.pool = mpool
	nv = nv.setvalsize(len(value)).setvalue(value)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		nv.setvalue(value)
	}
	mpool.free(ptr)
}

func BenchmarkGetValue(b *testing.B) {
	minblock, maxblock, capacity := 96, 1024*1024*10, 1024*1024*1024
	blocksize, value := 20*1024, make([]byte, 10*1024)

	marena := newmemarena(minblock, maxblock, capacity)
	ptr, mpool := marena.alloc(blocksize)
	nv := (*nodevalue)(ptr)
	nv.pool = mpool
	nv = nv.setvalsize(len(value)).setvalue(value)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		nv.value()
	}
	mpool.free(ptr)
}