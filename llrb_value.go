package llrb

import "unsafe"
import "reflect"

const nvaluesize = 8 // plus value size
type nodevalue struct {
	hdr1     uint64 // vsize(32)
	pool     *mempool
	valstart unsafe.Pointer // just a place-holder
}

func (nv *nodevalue) setvalsize(size int) *nodevalue {
	nv.hdr1 = (nv.hdr1 & 0xffffffff00000000) | (uint64(size) & 0xffffffff)
	return nv
}

func (nv *nodevalue) valsize() int {
	return int(nv.hdr1 & 0xffffffff)

}
func (nv *nodevalue) setvalue(val []byte) *nodevalue {
	var dst []byte
	sl := (*reflect.SliceHeader)(unsafe.Pointer(&dst))
	sl.Len = len(val)
	sl.Cap = len(val)
	sl.Data = (uintptr)(unsafe.Pointer(&nv.valstart))
	return nv.setvalsize(copy(dst, val))
}

func (nv *nodevalue) value() (val []byte) {
	sl := (*reflect.SliceHeader)(unsafe.Pointer(&val))
	sl.Len = nv.valsize()
	sl.Cap = sl.Len
	sl.Data = (uintptr)(unsafe.Pointer(&nv.valstart))
	return
}