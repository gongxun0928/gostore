package storage

import "sync/atomic"
import "unsafe"

//---- LLRB MVCC write operations.

// caller should free old-Llrbnode if it is not null.
func (llrb *LLRB) UpsertCow(k, v []byte) (newnd, oldnd *Llrbnode) {
	var root *Llrbnode
	var reclaim []*Llrbnode

	if k == nil {
		panic("upserting nil key")
	}
	nd := (*Llrbnode)(atomic.LoadPointer(&llrb.root))
	depth := int64(1) /*upsertdepth*/
	root, newnd, oldnd, reclaim = llrb.upsertCow(nd, depth, k, v, llrb.mvcc.reclaim)
	llrb.reclaimNodes("upsert", reclaim)
	llrb.mvcc.reclaim = llrb.mvcc.reclaim[:0]
	root.metadata().setblack()
	atomic.StorePointer(&llrb.root, unsafe.Pointer(root))
	if oldnd == nil {
		atomic.AddInt64(&llrb.count, 1)
	} else {
		atomic.AddInt64(&llrb.keymemory, -int64(len(oldnd.key())))
		atomic.AddInt64(&llrb.valmemory, -int64(len(oldnd.nodevalue().value())))
	}
	atomic.AddInt64(&llrb.keymemory, int64(len(k)))
	atomic.AddInt64(&llrb.valmemory, int64(len(v)))
	return newnd, oldnd
}

// returns root, newnd, oldnd
func (llrb *LLRB) upsertCow(
	nd *Llrbnode, depth int64,
	key, value []byte,
	reclaim []*Llrbnode) (*Llrbnode, *Llrbnode, *Llrbnode, []*Llrbnode) {

	var oldnd, newnd *Llrbnode

	if nd == nil {
		newnd := llrb.newnode(key, value)
		llrb.upsertdepth.add(depth)
		return newnd, newnd, nil, reclaim
	}
	reclaim = append(reclaim, nd)
	ndmvcc := llrb.clone(nd)

	ndmvcc = llrb.walkdownrot23Cow(ndmvcc)

	if ndmvcc.gtkey(key) {
		ndmvcc.left, newnd, oldnd, reclaim =
			llrb.upsertCow(ndmvcc.left, depth+1, key, value, reclaim)
	} else if ndmvcc.ltkey(key) {
		ndmvcc.right, newnd, oldnd, reclaim =
			llrb.upsertCow(ndmvcc.right, depth+1, key, value, reclaim)
	} else {
		oldnd = nd
		if nv := ndmvcc.nodevalue(); nv != nil { // free the value if present
			nv.pool.free(unsafe.Pointer(nv))
		}
		if value != nil { // and new value if need be
			ptr, mpool := llrb.valarena.alloc(int64(nvaluesize + len(value)))
			nv := (*nodevalue)(ptr)
			nv.pool = mpool
			ndmvcc = ndmvcc.setnodevalue(nv.setvalue(value))
		}
		newnd = ndmvcc
		llrb.upsertdepth.add(depth)
	}

	ndmvcc, reclaim = llrb.walkuprot23Cow(ndmvcc, reclaim)
	return ndmvcc, newnd, oldnd, reclaim
}

func (llrb *LLRB) DeleteMinCow() *Llrbnode {
	var reclaim []*Llrbnode

	nd := (*Llrbnode)(atomic.LoadPointer(&llrb.root))
	root, deleted, reclaim := llrb.deleteminCow(nd, llrb.mvcc.reclaim)
	llrb.reclaimNodes("delmin", reclaim)
	llrb.mvcc.reclaim = llrb.mvcc.reclaim[:0]
	if root != nil {
		root.metadata().setblack()
	}
	atomic.StorePointer(&llrb.root, unsafe.Pointer(root))
	if deleted != nil {
		atomic.AddInt64(&llrb.keymemory, -int64(len(deleted.key())))
		atomic.AddInt64(&llrb.valmemory, -int64(len(deleted.nodevalue().value())))
		atomic.AddInt64(&llrb.count, -1)
	}
	return deleted
}

// using 2-3 trees
func (llrb *LLRB) deleteminCow(
	nd *Llrbnode, reclaim []*Llrbnode) (*Llrbnode, *Llrbnode, []*Llrbnode) {

	var deleted *Llrbnode

	if nd == nil {
		return nil, nil, reclaim
	}
	if nd.left == nil {
		reclaim = append(reclaim, nd)
		return nil, nd, reclaim
	}

	reclaim = append(reclaim, nd)
	ndmvcc := llrb.clone(nd)

	if !isred(ndmvcc.left) && !isred(ndmvcc.left.left) {
		ndmvcc, reclaim = moveredleftCow(llrb, ndmvcc, reclaim)
	}

	ndmvcc.left, deleted, reclaim = llrb.deleteminCow(ndmvcc.left, reclaim)
	ndmvcc, reclaim = fixupCow(llrb, ndmvcc, reclaim)

	return ndmvcc, deleted, reclaim
}

func (llrb *LLRB) DeleteMaxCow() *Llrbnode {
	var reclaim []*Llrbnode

	nd := (*Llrbnode)(atomic.LoadPointer(&llrb.root))
	root, deleted, _ := llrb.deletemaxCow(nd, llrb.mvcc.reclaim)
	llrb.reclaimNodes("delmax", reclaim)
	llrb.mvcc.reclaim = llrb.mvcc.reclaim[:0]
	if root != nil {
		root.metadata().setblack()
	}
	atomic.StorePointer(&llrb.root, unsafe.Pointer(root))
	if deleted != nil {
		atomic.AddInt64(&llrb.keymemory, -int64(len(deleted.key())))
		atomic.AddInt64(&llrb.valmemory, -int64(len(deleted.nodevalue().value())))
		atomic.AddInt64(&llrb.count, -1)
	}
	return deleted
}

// using 2-3 trees
func (llrb *LLRB) deletemaxCow(
	nd *Llrbnode, reclaim []*Llrbnode) (*Llrbnode, *Llrbnode, []*Llrbnode) {

	var deleted *Llrbnode

	if nd == nil {
		return nil, nil, reclaim
	}
	reclaim = append(reclaim, nd)
	ndmvcc := llrb.clone(nd)

	if isred(ndmvcc.left) {
		ndmvcc, reclaim = rotaterightCow(llrb, ndmvcc, reclaim)
	}
	if ndmvcc.right == nil {
		return nil, ndmvcc, reclaim
	}

	if !isred(ndmvcc.right) && !isred(ndmvcc.right.left) {
		ndmvcc, reclaim = moveredrightCow(llrb, ndmvcc, reclaim)
	}

	ndmvcc.right, deleted, reclaim = llrb.deletemaxCow(ndmvcc.right, reclaim)
	ndmvcc, reclaim = fixupCow(llrb, ndmvcc, reclaim)

	return ndmvcc, deleted, reclaim
}

func (llrb *LLRB) DeleteCow(key []byte) *Llrbnode {
	var reclaim []*Llrbnode

	nd := (*Llrbnode)(atomic.LoadPointer(&llrb.root))
	root, deleted := llrb.delete(nd, key)
	llrb.reclaimNodes("delete", reclaim)
	llrb.mvcc.reclaim = llrb.mvcc.reclaim[:0]
	if root != nil {
		root.metadata().setblack()
	}
	atomic.StorePointer(&llrb.root, unsafe.Pointer(root))
	if deleted != nil {
		atomic.AddInt64(&llrb.keymemory, -int64(len(deleted.key())))
		atomic.AddInt64(&llrb.valmemory, -int64(len(deleted.nodevalue().value())))
		atomic.AddInt64(&llrb.count, -1)
	}
	return deleted
}

func (llrb *LLRB) deleteCow(
	nd *Llrbnode, key []byte,
	reclaim []*Llrbnode) (*Llrbnode, *Llrbnode, []*Llrbnode) {

	var newnd, deleted *Llrbnode

	if nd == nil {
		return nil, nil, reclaim
	}
	reclaim = append(reclaim, nd)
	ndmvcc := llrb.clone(nd)

	if ndmvcc.gtkey(key) {
		if ndmvcc.left == nil { // key not present. Nothing to delete
			return ndmvcc, nil, reclaim
		}
		if !isred(ndmvcc.left) && !isred(ndmvcc.left.left) {
			ndmvcc, reclaim = moveredleftCow(llrb, ndmvcc, reclaim)
		}
		ndmvcc.left, deleted, reclaim = llrb.deleteCow(ndmvcc.left, key, reclaim)

	} else {
		if isred(ndmvcc.left) {
			ndmvcc, reclaim = rotaterightCow(llrb, ndmvcc, reclaim)
		}

		// If @key equals @h.Item and no right children at @h
		if !ndmvcc.ltkey(key) && ndmvcc.right == nil {
			return nil, ndmvcc, reclaim
		}

		if ndmvcc.right != nil &&
			!isred(ndmvcc.right) && !isred(ndmvcc.right.left) {

			ndmvcc, reclaim = moveredrightCow(llrb, ndmvcc, reclaim)
		}

		// If @key equals @h.Item, and (from above) 'h.Right != nil'
		if !ndmvcc.ltkey(key) {
			var subdeleted *Llrbnode
			ndmvcc.right, subdeleted, reclaim =
				llrb.deleteminCow(ndmvcc.right, reclaim)
			if subdeleted == nil {
				panic("logic")
			}
			newnd = llrb.clone(subdeleted)
			newnd.left, newnd.right = ndmvcc.left, ndmvcc.right
			if ndmvcc.metadata().isdirty() {
				newnd.metadata().setdirty()
			}
			if ndmvcc.metadata().isblack() {
				newnd.metadata().setblack()
			} else {
				newnd.metadata().setred()
			}
			newnd.nodevalue().setvalue(subdeleted.nodevalue().value())
			deleted, ndmvcc = ndmvcc, newnd
			llrb.Freenode(subdeleted)
		} else { // Else, @key is bigger than @ndmvcc
			ndmvcc.right, deleted, reclaim =
				llrb.deleteCow(ndmvcc.right, key, reclaim)
		}
	}
	ndmvcc, reclaim = fixupCow(llrb, ndmvcc, reclaim)
	return ndmvcc, deleted, reclaim
}

// rotation driver routines for 2-3 algorithm - mvcc

func (llrb *LLRB) walkdownrot23Cow(nd *Llrbnode) *Llrbnode {
	return nd
}

func (llrb *LLRB) walkuprot23Cow(
	nd *Llrbnode, reclaim []*Llrbnode) (*Llrbnode, []*Llrbnode) {

	if isred(nd.right) && !isred(nd.left) {
		nd, reclaim = rotateleftCow(llrb, nd, reclaim)
	}

	if isred(nd.left) && isred(nd.left.left) {
		nd, reclaim = rotaterightCow(llrb, nd, reclaim)
	}

	if isred(nd.left) && isred(nd.right) {
		reclaim = flipCow(llrb, nd, reclaim)
	}

	return nd, reclaim
}

func (llrb *LLRB) reclaimNodes(opname string, reclaim []*Llrbnode) {
	llrb.mvcc.cowednodes = append(llrb.mvcc.cowednodes, reclaim...)
	llrb.mvcc.reclaimstats[opname].add(int64(len(reclaim)))
}

// snapshotting

type LLRBSnapshot struct {
	llrb     *LLRB
	root     unsafe.Pointer
	reclaim  []*Llrbnode
	next     unsafe.Pointer // *LLRBSnapshot
	refcount int32
}

func (llrb *LLRB) NewSnapshot() *LLRBSnapshot {
	location := &llrb.mvcc.readerhd
	reference := atomic.LoadPointer(location)
	for reference != nil {
		location = &((*LLRBSnapshot)(reference).next)
		reference = atomic.LoadPointer(location)
	}
	snapshot := &LLRBSnapshot{
		llrb: llrb,
		root: atomic.LoadPointer(&llrb.root),
	}
	if len(llrb.mvcc.cowednodes) > 0 {
		snapshot.reclaim = make([]*Llrbnode, len(llrb.mvcc.cowednodes))
	}
	copy(snapshot.reclaim, llrb.mvcc.cowednodes)
	llrb.mvcc.cowednodes = llrb.mvcc.cowednodes[:0]
	atomic.StorePointer(location, unsafe.Pointer(snapshot))
	return snapshot
}

func (snapshot *LLRBSnapshot) RefCount() {
	atomic.AddInt32(&snapshot.refcount, 1)
}

func (snapshot *LLRBSnapshot) Release() {
	atomic.AddInt32(&snapshot.refcount, -1)
	if atomic.LoadInt32(&snapshot.refcount) == 0 {
	}
}

func (snapshot *LLRBSnapshot) Destroy() {
	llrb := snapshot.llrb
	for _, nd := range snapshot.reclaim {
		llrb.Freenode(nd)
	}
}
