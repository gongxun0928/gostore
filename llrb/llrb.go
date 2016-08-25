package llrb

import "fmt"
import "time"
import "unsafe"
import "io"
import "strings"
import "bytes"
import "sync"
import "sync/atomic"

import "github.com/prataprc/storage.go/lib"
import "github.com/prataprc/storage.go/api"
import "github.com/prataprc/storage.go/log"
import humanize "github.com/dustin/go-humanize"

// LLRB to manage in-memory sorted index using left-leaning-red-black trees.
type LLRB struct { // tree container
	// 64-bit aligned reader statistics
	n_lookups int64
	n_ranges  int64

	// 64-bit aligned writer statistics
	n_count   int64 // number of nodes in the tree
	n_inserts int64
	n_updates int64
	n_deletes int64
	n_nodes   int64
	n_frees   int64
	n_clones  int64
	keymemory int64 // memory used by all keys
	valmemory int64 // memory used by all values

	// mvcc
	mvcc struct {
		// 64-bit aligned
		ismut int64
		// statistics
		n_snapshots int64
		n_purgedss  int64
		n_activess  int64
		n_cclookups int64
		n_ccranges  int64

		// can be unaligned fields

		enabled    bool
		reclaim    []*Llrbnode
		writer     *LLRBWriter
		snapshot   *LLRBSnapshot
		n_reclaims int64
		h_bulkfree *lib.HistogramInt64
		h_reclaims map[string]*lib.HistogramInt64
		h_versions *lib.HistogramInt64
	}

	h_upsertdepth *lib.HistogramInt64

	// can be unaligned fields

	name       string
	nodearena  api.Mallocer
	valarena   api.Mallocer
	root       *Llrbnode
	borntime   time.Time
	dead       bool
	clock      *vectorclock // current clock
	rw         sync.RWMutex
	iterpool   chan *iterator
	activeiter int64

	// settings
	fmask     metadataMask // only 12 bits
	mdsize    int
	maxvb     int64
	setts     lib.Settings
	logprefix string
	memratio  float64

	// scratch pad
	strsl []string
}

// NewLLRB a new instance of in-memory sorted index.
func NewLLRB(name string, setts lib.Settings) *LLRB {
	setts = make(lib.Settings).Mixin(DefaultSettings(), setts)

	llrb := &LLRB{name: name, borntime: time.Now()}
	llrb.iterpool = make(chan *iterator, setts.Int64("iterpool.size"))

	llrb.validateSettings(setts)

	llrb.maxvb = setts.Int64("maxvb")
	llrb.clock = newvectorclock(llrb.maxvb)

	// setup arena for nodes and node-values.
	llrb.nodearena = llrb.newnodearena(setts)
	llrb.valarena = llrb.newvaluearena(setts)

	llrb.logprefix = fmt.Sprintf("[LLRB-%s]", name)

	// set up metadata options
	llrb.fmask = llrb.setupfmask(setts)
	llrb.mdsize = (&metadata{}).initMetadata(0, llrb.fmask).sizeof()
	llrb.setts = setts

	// statistics
	llrb.h_upsertdepth = lib.NewhistorgramInt64(1, 256, 1)

	// scratch pads
	llrb.strsl = make([]string, 0)
	llrb.memratio = 0.4 // (keymemory / allocated) for each arena

	// mvcc
	llrb.mvcc.enabled = setts.Bool("mvcc.enable")
	if llrb.mvcc.enabled {
		llrb.mvcc.reclaim = make([]*Llrbnode, 0, 64)
		llrb.mvcc.h_bulkfree = lib.NewhistorgramInt64(200000, 500000, 100000)
		llrb.mvcc.h_reclaims = map[string]*lib.HistogramInt64{
			"upsert":    lib.NewhistorgramInt64(4, 1024, 4),
			"mutations": lib.NewhistorgramInt64(4, 1024, 4),
			"delmin":    lib.NewhistorgramInt64(4, 1024, 4),
			"delmax":    lib.NewhistorgramInt64(4, 1024, 4),
			"delete":    lib.NewhistorgramInt64(4, 1024, 4),
		}
		llrb.mvcc.h_versions = lib.NewhistorgramInt64(0, 32, 1)
		llrb.spawnwriter()
	}

	log.Infof("%v started ...\n", llrb.logprefix)
	llrb.logsettings(setts)
	return llrb
}

// SetMemratio for validating memory consumption. Set this to minimum expected
// ratio of keymemory / allocated, before calling llrb.Validate().
func (llrb *LLRB) SetMemratio(memratio float64) {
	llrb.memratio = memratio
}

// ---- Index{} interface

// ID implement Index{} interface.
func (llrb *LLRB) ID() string {
	return llrb.name
}

// Count implement Index{} interface.
func (llrb *LLRB) Count() int64 {
	return atomic.LoadInt64(&llrb.n_count)
}

// Isactive implement Index{} interface.
func (llrb *LLRB) Isactive() bool {
	return llrb.dead == false
}

// Refer implement Snapshot{} interface. Call this method on llrb-snapshot,
// calling on this type will cause panic.
func (llrb *LLRB) Refer() {
	panic("Refer(): only allowed on snapshot")
}

// Release implement Snapshot{} interface. Call this method on llrb-snapshot,
// calling on this type will cause panic.
func (llrb *LLRB) Release() {
	panic("Release(): only allowed on snapshot")
}

// RSnapshot implement Index{} interface.
func (llrb *LLRB) RSnapshot(snapch chan api.IndexSnapshot) error {
	if llrb.mvcc.enabled {
		err := llrb.mvcc.writer.getSnapshot(snapch)
		if err != nil {
			return err
		}
		return nil
	}
	panic("RSnapshot(): mvcc is not enabled")
}

// Destroy implement Index{} interface.
func (llrb *LLRB) Destroy() error {
	if atomic.LoadInt64(&llrb.activeiter) > 0 {
		return api.ErrorActiveIterators
	}
	if llrb.dead == false {
		if llrb.mvcc.enabled {
			llrb.mvcc.writer.destroy()
			llrb.mvcc.reclaim, llrb.mvcc.writer = nil, nil
			llrb.mvcc.h_reclaims = nil
		}
		llrb.nodearena.Release()
		llrb.valarena.Release()
		llrb.root, llrb.clock = nil, nil
		llrb.setts, llrb.strsl = nil, nil
		llrb.dead = true
		return nil
	}
	panic("Destroy(): already dead tree")
}

// Stats implement Indexer{} interface.
func (llrb *LLRB) Stats() (map[string]interface{}, error) {
	if llrb.mvcc.enabled {
		return llrb.mvcc.writer.stats()
	}
	llrb.rw.RLock()
	defer llrb.rw.RUnlock()
	return llrb.stats()
}

// Fullstats implement Indexer{} interface.
func (llrb *LLRB) Fullstats() (map[string]interface{}, error) {
	if llrb.mvcc.enabled {
		return llrb.mvcc.writer.fullstats()
	}

	llrb.rw.RLock()
	defer llrb.rw.RUnlock()
	return llrb.fullstats()
}

// Validate implement Indexer{} interface.
func (llrb *LLRB) Validate() {
	if llrb.mvcc.enabled {
		if err := llrb.mvcc.writer.validate(); err != nil {
			panic(fmt.Errorf("Validate(): %v", err))
		}
		return
	}
	llrb.rw.RLock()
	llrb.validate(llrb.root)
	llrb.rw.RUnlock()
}

// Log implement Indexer{} interface.
func (llrb *LLRB) Log(involved int, humanize bool) {
	if llrb.mvcc.enabled {
		llrb.mvcc.writer.log(involved, humanize)
		return
	}
	llrb.rw.RLock()
	defer llrb.rw.RUnlock()
	llrb.log(involved, humanize)
}

//---- IndexReader{} interface.

// Has implement IndexReader{} interface.
func (llrb *LLRB) Has(key []byte) bool {
	if llrb.mvcc.enabled {
		panic("Has(): mvcc enabled, use snapshots for reading")
	}
	return llrb.Get(key, nil)
}

// Get implement IndexReader{} interface.
func (llrb *LLRB) Get(key []byte, callb api.NodeCallb) bool {
	if llrb.mvcc.enabled {
		panic("Get(): mvcc enabled, use snapshots for reading")
	}

	llrb.rw.RLock()
	defer llrb.rw.RUnlock()
	defer atomic.AddInt64(&llrb.n_lookups, 1)

	if nd := llrb.get(key); nd != nil {
		if callb == nil {
			return true
		}
		return callb(llrb, 0, nd, nd)
	}
	return false
}

func (llrb *LLRB) get(key []byte) api.Node {
	nd := llrb.root
	for nd != nil {
		if nd.gtkey(llrb.mdsize, key, false) {
			nd = nd.left
		} else if nd.ltkey(llrb.mdsize, key, false) {
			nd = nd.right
		} else {
			return nd
		}
	}
	return nil // key is not present in the tree
}

// Min implement IndexReader{} interface.
func (llrb *LLRB) Min(callb api.NodeCallb) bool {
	if llrb.mvcc.enabled {
		panic("Min(): mvcc enabled, use snapshots for reading")
	}

	llrb.rw.RLock()
	defer llrb.rw.RUnlock()
	defer atomic.AddInt64(&llrb.n_lookups, 1)

	if nd := llrb.min(); nd != nil {
		if callb == nil {
			return true
		}
		return callb(llrb, 0, nd, nd)
	}
	return false
}

func (llrb *LLRB) min() api.Node {
	var nd *Llrbnode
	if nd = llrb.root; nd == nil {
		return nil
	}
	for nd.left != nil {
		nd = nd.left
	}
	return nd
}

// Max implement IndexReader{} interface.
func (llrb *LLRB) Max(callb api.NodeCallb) bool {
	if llrb.mvcc.enabled {
		panic("Max(): mvcc enabled, use snapshots for reading")
	}

	llrb.rw.RLock()
	defer llrb.rw.RUnlock()
	defer atomic.AddInt64(&llrb.n_lookups, 1)

	if nd := llrb.max(); nd != nil {
		if callb == nil {
			return true
		}
		return callb(llrb, 0, nd, nd)
	}
	return false
}

func (llrb *LLRB) max() api.Node {
	var nd *Llrbnode
	if nd = llrb.root; nd == nil {
		return nil
	}
	for nd.right != nil {
		nd = nd.right
	}
	return nd
}

// Range from lkey to hkey, incl can be "both", "low", "high", "none"
func (llrb *LLRB) Range(lkey, hkey []byte, incl string, reverse bool, iter api.NodeCallb) {
	if llrb.mvcc.enabled {
		panic("Range(): mvcc enabled, use snapshots for reading")
	}

	lkey, hkey = llrb.fixrangeargs(lkey, hkey)
	if lkey != nil && hkey != nil && bytes.Compare(lkey, hkey) == 0 {
		if incl == "none" {
			return
		} else if incl == "low" || incl == "high" {
			incl = "both"
		}
	}

	llrb.rw.RLock()

	if reverse {
		switch incl {
		case "both":
			llrb.rvrslehe(llrb.root, lkey, hkey, iter)
		case "high":
			llrb.rvrsleht(llrb.root, lkey, hkey, iter)
		case "low":
			llrb.rvrslthe(llrb.root, lkey, hkey, iter)
		default:
			llrb.rvrsltht(llrb.root, lkey, hkey, iter)
		}

	} else {
		switch incl {
		case "both":
			llrb.rangehele(llrb.root, lkey, hkey, iter)
		case "high":
			llrb.rangehtle(llrb.root, lkey, hkey, iter)
		case "low":
			llrb.rangehelt(llrb.root, lkey, hkey, iter)
		default:
			llrb.rangehtlt(llrb.root, lkey, hkey, iter)
		}
	}

	llrb.rw.RUnlock()
	atomic.AddInt64(&llrb.n_ranges, 1)
}

// Iterate implement IndexReader{} interface.
func (llrb *LLRB) Iterate(lkey, hkey []byte, incl string, r bool) api.IndexIterator {
	if llrb.mvcc.enabled {
		panic("Iterate(): mvcc enabled, use snapshots for reading")
	}

	lkey, hkey = llrb.fixrangeargs(lkey, hkey)
	if lkey != nil && hkey != nil && bytes.Compare(lkey, hkey) == 0 {
		if incl == "none" {
			return nil
		} else if incl == "low" || incl == "high" {
			incl = "both"
		}
	}

	llrb.rw.RLock()

	var iter *iterator
	select {
	case iter = <-llrb.iterpool:
	default:
		iter = &iterator{}
	}

	// NOTE: always re-initialize, because we are getting it back from pool.
	iter.tree, iter.llrb = llrb, llrb
	iter.nodes, iter.index, iter.limit = iter.nodes[:0], 0, 5
	iter.continuate = false
	iter.startkey, iter.endkey, iter.incl, iter.reverse = lkey, hkey, incl, r
	iter.closed, iter.activeiter = false, &llrb.activeiter

	if iter.nodes == nil {
		iter.nodes = make([]api.Node, 0)
	}

	iter.rangefill()
	if r {
		switch iter.incl {
		case "none":
			iter.incl = "high"
		case "low":
			iter.incl = "both"
		}
	} else {
		switch iter.incl {
		case "none":
			iter.incl = "low"
		case "high":
			iter.incl = "both"
		}
	}

	atomic.AddInt64(&llrb.n_ranges, 1)
	atomic.AddInt64(&llrb.activeiter, 1)
	return iter
}

//---- IndexWriter{} interface

// Upsert implement IndexWriter{} interface.
func (llrb *LLRB) Upsert(key, value []byte, callb api.NodeCallb) error {
	if key == nil {
		panic("Upsert(): upserting nil key")
	}

	if llrb.mvcc.enabled {
		return llrb.mvcc.writer.wupsert(key, value, callb)
	}

	llrb.rw.Lock()

	root, newnd, oldnd := llrb.upsert(llrb.root, 1 /*depth*/, key, value)
	root.metadata().setblack()
	llrb.root = root
	llrb.upsertcounts(key, value, oldnd)

	if callb != nil {
		callb(llrb, 0, llndornil(newnd), llndornil(oldnd))
	}
	newnd.metadata().cleardirty()
	llrb.freenode(oldnd)

	llrb.rw.Unlock()
	return nil
}

// Mutations implement IndexWriter{} interface.
func (llrb *LLRB) Mutations(cmds []byte, keys, values [][]byte, callb api.NodeCallb) error {
	if llrb.mvcc.enabled {
		return llrb.mvcc.writer.wmutations(cmds, keys, values, callb)
	}

	var i int
	var cmd byte

	localfn := func(index api.Index, _ int64, newnd, oldnd api.Node) bool {
		if callb != nil {
			callb(index, int64(i), newnd, oldnd)
		}
		return false
	}

	for i, cmd = range cmds {
		key, value := keys[i], values[i]
		switch cmd {
		case api.UpsertCmd:
			llrb.Upsert(key, value, localfn)
		case api.DelminCmd:
			llrb.DeleteMin(localfn)
		case api.DelmaxCmd:
			llrb.DeleteMax(localfn)
		case api.DeleteCmd:
			llrb.Delete(key, localfn)
		}
	}
	return nil
}

// returns root, newnd, oldnd
func (llrb *LLRB) upsert(
	nd *Llrbnode, depth int64,
	key, value []byte) (*Llrbnode, *Llrbnode, *Llrbnode) {

	var oldnd, newnd *Llrbnode
	var dirty bool

	if nd == nil {
		newnd := llrb.newnode(key, value)
		llrb.h_upsertdepth.Add(depth)
		return newnd, newnd, nil
	}

	nd = llrb.walkdownrot23(nd)

	if nd.gtkey(llrb.mdsize, key, false) {
		nd.left, newnd, oldnd = llrb.upsert(nd.left, depth+1, key, value)
	} else if nd.ltkey(llrb.mdsize, key, false) {
		nd.right, newnd, oldnd = llrb.upsert(nd.right, depth+1, key, value)
	} else {
		oldnd, dirty = llrb.clone(nd), false
		if nd.metadata().ismvalue() {
			if nv := nd.nodevalue(); nv != nil { // free the value if present
				nv.pool.Free(unsafe.Pointer(nv))
				nd, dirty = nd.setnodevalue(nil), true
			}
		}
		if nd.metadata().ismvalue() && len(value) > 0 { // add new value if req.
			ptr, mpool := llrb.valarena.Alloc(int64(nvaluesize + len(value)))
			nv := (*nodevalue)(ptr)
			nv.pool = mpool
			nd, dirty = nd.setnodevalue(nv.setvalue(value)), true
		}
		newnd = nd
		if dirty {
			nd.metadata().setdirty()
		}
		llrb.h_upsertdepth.Add(depth)
	}

	nd = llrb.walkuprot23(nd)
	return nd, newnd, oldnd
}

// DeleteMin implement IndexWriter{} interface.
func (llrb *LLRB) DeleteMin(callb api.NodeCallb) error {
	if llrb.mvcc.enabled {
		return llrb.mvcc.writer.wdeleteMin(callb)
	}

	llrb.rw.Lock()

	root, deleted := llrb.deletemin(llrb.root)
	if root != nil {
		root.metadata().setblack()
	}
	llrb.root = root

	llrb.delcount(deleted)

	if callb != nil {
		nd := llndornil(deleted)
		callb(llrb, 0, nd, nd)
	}
	llrb.freenode(deleted)
	llrb.rw.Unlock()
	return nil
}

// using 2-3 trees
func (llrb *LLRB) deletemin(nd *Llrbnode) (newnd, deleted *Llrbnode) {
	if nd == nil {
		return nil, nil
	}
	if nd.left == nil {
		return nil, nd
	}
	if !isred(nd.left) && !isred(nd.left.left) {
		nd = llrb.moveredleft(nd)
	}
	nd.left, deleted = llrb.deletemin(nd.left)
	return llrb.fixup(nd), deleted
}

// DeleteMax implements IndexWriter{} interface.
func (llrb *LLRB) DeleteMax(callb api.NodeCallb) error {
	if llrb.mvcc.enabled {
		return llrb.mvcc.writer.wdeleteMax(callb)
	}

	llrb.rw.Lock()

	root, deleted := llrb.deletemax(llrb.root)
	if root != nil {
		root.metadata().setblack()
	}
	llrb.root = root

	llrb.delcount(deleted)

	if callb != nil {
		nd := llndornil(deleted)
		callb(llrb, 0, nd, nd)
	}
	llrb.freenode(deleted)

	llrb.rw.Unlock()
	return nil
}

// using 2-3 trees
func (llrb *LLRB) deletemax(nd *Llrbnode) (newnd, deleted *Llrbnode) {
	if nd == nil {
		return nil, nil
	}
	if isred(nd.left) {
		nd = llrb.rotateright(nd)
	}
	if nd.right == nil {
		return nil, nd
	}
	if !isred(nd.right) && !isred(nd.right.left) {
		nd = llrb.moveredright(nd)
	}
	nd.right, deleted = llrb.deletemax(nd.right)
	return llrb.fixup(nd), deleted
}

// Delete implement IndexWriter{} interface.
func (llrb *LLRB) Delete(key []byte, callb api.NodeCallb) error {
	if llrb.mvcc.enabled {
		return llrb.mvcc.writer.wdelete(key, callb)
	}

	llrb.rw.Lock()

	root, deleted := llrb.delete(llrb.root, key)
	if root != nil {
		root.metadata().setblack()
	}
	llrb.root = root

	llrb.delcount(deleted)

	if callb != nil {
		nd := llndornil(deleted)
		callb(llrb, 0, nd, nd)
	}
	llrb.freenode(deleted)

	llrb.rw.Unlock()
	return nil
}

func (llrb *LLRB) delete(nd *Llrbnode, key []byte) (newnd, deleted *Llrbnode) {
	if nd == nil {
		return nil, nil
	}

	if nd.gtkey(llrb.mdsize, key, false) {
		if nd.left == nil { // key not present. Nothing to delete
			return nd, nil
		}
		if !isred(nd.left) && !isred(nd.left.left) {
			nd = llrb.moveredleft(nd)
		}
		nd.left, deleted = llrb.delete(nd.left, key)

	} else {
		if isred(nd.left) {
			nd = llrb.rotateright(nd)
		}
		// If @key equals @h.Item and no right children at @h
		if !nd.ltkey(llrb.mdsize, key, false) && nd.right == nil {
			return nil, nd
		}
		if nd.right != nil && !isred(nd.right) && !isred(nd.right.left) {
			nd = llrb.moveredright(nd)
		}
		// If @key equals @h.Item, and (from above) 'h.Right != nil'
		if !nd.ltkey(llrb.mdsize, key, false) {
			var subdeleted *Llrbnode
			nd.right, subdeleted = llrb.deletemin(nd.right)
			if subdeleted == nil {
				panic("delete(): fatal logic, call the programmer")
			}
			newnd := llrb.clone(subdeleted)
			newnd.left, newnd.right = nd.left, nd.right
			if nd.metadata().isdirty() {
				//newnd.metadata().setdirty()
				panic("delete(): unexpected dirty node, call the programmer")
			}
			if nd.metadata().isblack() {
				newnd.metadata().setblack()
			} else {
				newnd.metadata().setred()
			}
			if newnd.metadata().ismvalue() {
				newnd.nodevalue().setvalue(subdeleted.nodevalue().value())
			}
			deleted, nd = nd, newnd
			llrb.freenode(subdeleted)
		} else { // Else, @key is bigger than @nd
			nd.right, deleted = llrb.delete(nd.right, key)
		}
	}
	return llrb.fixup(nd), deleted
}

// rotation routines for 2-3 algorithm

func (llrb *LLRB) walkdownrot23(nd *Llrbnode) *Llrbnode {
	return nd
}

func (llrb *LLRB) walkuprot23(nd *Llrbnode) *Llrbnode {
	if isred(nd.right) && !isred(nd.left) {
		nd = llrb.rotateleft(nd)
	}
	if isred(nd.left) && isred(nd.left.left) {
		nd = llrb.rotateright(nd)
	}
	if isred(nd.left) && isred(nd.right) {
		llrb.flip(nd)
	}
	return nd
}

func (llrb *LLRB) rotateleft(nd *Llrbnode) *Llrbnode {
	y := nd.right
	if y.metadata().isblack() {
		panic("rotateleft(): rotating a black link ? call the programmer")
	}
	nd.right = y.left
	y.left = nd
	if nd.metadata().isblack() {
		y.metadata().setblack()
	} else {
		y.metadata().setred()
	}
	nd.metadata().setred()
	return y
}

func (llrb *LLRB) rotateright(nd *Llrbnode) *Llrbnode {
	x := nd.left
	if x.metadata().isblack() {
		panic("rotateright(): rotating a black link ? call the programmer")
	}
	nd.left = x.right
	x.right = nd
	if nd.metadata().isblack() {
		x.metadata().setblack()
	} else {
		x.metadata().setred()
	}
	nd.metadata().setred()
	return x
}

// REQUIRE: Left and Right children must be present
func (llrb *LLRB) flip(nd *Llrbnode) {
	nd.left.metadata().togglelink()
	nd.right.metadata().togglelink()
	nd.metadata().togglelink()
}

// REQUIRE: Left and Right children must be present
func (llrb *LLRB) moveredleft(nd *Llrbnode) *Llrbnode {
	llrb.flip(nd)
	if isred(nd.right.left) {
		nd.right = llrb.rotateright(nd.right)
		nd = llrb.rotateleft(nd)
		llrb.flip(nd)
	}
	return nd
}

// REQUIRE: Left and Right children must be present
func (llrb *LLRB) moveredright(nd *Llrbnode) *Llrbnode {
	llrb.flip(nd)
	if isred(nd.left.left) {
		nd = llrb.rotateright(nd)
		llrb.flip(nd)
	}
	return nd
}

func (llrb *LLRB) fixup(nd *Llrbnode) *Llrbnode {
	if isred(nd.right) {
		nd = llrb.rotateleft(nd)
	}
	if isred(nd.left) && isred(nd.left.left) {
		nd = llrb.rotateright(nd)
	}
	if isred(nd.left) && isred(nd.right) {
		llrb.flip(nd)
	}
	return nd
}

// Dotdump to convert whole tree into dot script that can be visualized using
// graphviz.
func (llrb *LLRB) Dotdump(buffer io.Writer) {
	lines := []string{
		"digraph llrb {",
		"  node[shape=record];\n",
		"}",
	}
	buffer.Write([]byte(strings.Join(lines[:len(lines)-1], "\n")))
	llrb.root.dotdump(buffer)
	buffer.Write([]byte(lines[len(lines)-1]))
}

//---- local functions

func (llrb *LLRB) newnode(k, v []byte) *Llrbnode {
	ptr, mpool := llrb.nodearena.Alloc(int64(nodesize + llrb.mdsize + len(k)))
	nd := (*Llrbnode)(ptr)
	nd.metadata().initMetadata(0, llrb.fmask).setdirty().setred()
	nd.setkey(llrb.mdsize, k)
	nd.pool, nd.left, nd.right = mpool, nil, nil

	if v != nil && nd.metadata().ismvalue() {
		ptr, mpool = llrb.valarena.Alloc(int64(nvaluesize + len(v)))
		nv := (*nodevalue)(ptr)
		nv.pool = mpool
		nvarg := (uintptr)(unsafe.Pointer(nv.setvalue(v)))
		nd.metadata().setmvalue((uint64)(nvarg))
	} else if v != nil {
		panic("newnode(): llrb tree not settings for accepting value")
	}

	llrb.n_nodes++
	return nd
}

func (llrb *LLRB) freenode(nd *Llrbnode) {
	if nd != nil {
		if nd.metadata().ismvalue() {
			nv := nd.nodevalue()
			if nv != nil {
				nv.pool.Free(unsafe.Pointer(nv))
			}
		}
		nd.pool.Free(unsafe.Pointer(nd))
		llrb.n_frees++
	}
}

func (llrb *LLRB) clone(nd *Llrbnode) (newnd *Llrbnode) {
	// clone Llrbnode.
	newndptr, mpool := llrb.nodearena.Alloc(nd.pool.Chunksize())
	newnd = (*Llrbnode)(newndptr)
	lib.Memcpy(unsafe.Pointer(newnd), unsafe.Pointer(nd), int(nd.pool.Chunksize()))
	newnd.pool = mpool
	// clone value if value is present.
	if nd.metadata().ismvalue() {
		if mvalue := nd.metadata().mvalue(); mvalue != 0 {
			nv := (*nodevalue)(unsafe.Pointer((uintptr)(mvalue)))
			newnvptr, mpool := llrb.valarena.Alloc(nv.pool.Chunksize())
			lib.Memcpy(newnvptr, unsafe.Pointer(nv), int(nv.pool.Chunksize()))
			newnv := (*nodevalue)(newnvptr)
			newnv.pool = mpool
			newnd.setnodevalue(newnv)
		}
	}
	llrb.n_clones++
	return
}

func (llrb *LLRB) upsertcounts(key, value []byte, oldnd *Llrbnode) {
	if oldnd == nil {
		llrb.n_count++
		llrb.n_inserts++
	} else {
		llrb.keymemory -= int64(len(oldnd.key(llrb.mdsize)))
		if oldnd.metadata().ismvalue() {
			if nv := oldnd.nodevalue(); nv != nil {
				llrb.valmemory -= int64(len(nv.value()))
			}
		}
		llrb.n_updates++
	}
	llrb.keymemory += int64(len(key))
	llrb.valmemory += int64(len(value))
}

func (llrb *LLRB) delcount(nd *Llrbnode) {
	if nd != nil {
		llrb.keymemory -= int64(len(nd.key(llrb.mdsize)))
		if nd.metadata().ismvalue() {
			if nv := nd.nodevalue(); nv != nil {
				llrb.valmemory -= int64(len(nv.value()))
			}
		}
		llrb.n_count--
		llrb.n_deletes++
	}
}

func (llrb *LLRB) fixrangeargs(lk, hk []byte) ([]byte, []byte) {
	l, h := lk, hk
	if len(lk) == 0 {
		l = nil
	}
	if len(hk) == 0 {
		h = nil
	}
	return l, h
}

func (llrb *LLRB) equivalent(n1, n2 *Llrbnode) bool {
	md1, md2 := n1.metadata(), n2.metadata()
	if md1.isdirty() != md2.isdirty() {
		//fmt.Println("dirty mismatch")
		return false
	} else if md1.isblack() != md2.isblack() {
		//fmt.Println("black mismatch")
		return false
	} else if md1.vbno() != md2.vbno() {
		//fmt.Println("vbno mismatch")
		return false
	} else if md1.isvbuuid() && (md1.vbuuid() != md2.vbuuid()) {
		//fmt.Println("vbuuid mismatch")
		return false
	} else if md1.isbnseq() && (md1.bnseq() != md2.bnseq()) {
		//fmt.Println("isbnseq mismatch")
		return false
	} else if md1.access() != md2.access() {
		//fmt.Println("access mismatch", md1.access())
		return false
	} else if n1.left != n2.left || n1.right != n2.right {
		//fmt.Println("left mismatch")
		return false
	} else if bytes.Compare(n1.key(llrb.mdsize), n2.key(llrb.mdsize)) != 0 {
		//fmt.Println("key mismatch")
		return false
	} else if md1.ismvalue() {
		if bytes.Compare(n1.nodevalue().value(), n2.nodevalue().value()) != 0 {
			//fmt.Println("dirty mismatch")
			return false
		}
	}
	return true
}

func (llrb *LLRB) logsettings(setts lib.Settings) {
	// key arena
	stats, err := llrb.stats()
	if err != nil {
		panic(fmt.Errorf("logsettings(): %v", err))
	}
	kblocks := len(stats["node.blocks"].([]int64))
	min := humanize.Bytes(uint64(llrb.setts.Int64("nodearena.minblock")))
	max := humanize.Bytes(uint64(llrb.setts.Int64("nodearena.maxblock")))
	cp := humanize.Bytes(uint64(llrb.setts.Int64("nodearena.capacity")))
	pcp := humanize.Bytes(uint64(llrb.setts.Int64("nodearena.pool.capacity")))
	fmsg := "%v key arena %v blocks over {%v %v} cap %v poolcap %v\n"
	log.Infof(fmsg, llrb.logprefix, kblocks, min, max, cp, pcp)

	// value arena
	vblocks := len(stats["value.blocks"].([]int64))
	min = humanize.Bytes(uint64(llrb.setts.Int64("valarena.minblock")))
	max = humanize.Bytes(uint64(llrb.setts.Int64("valarena.maxblock")))
	cp = humanize.Bytes(uint64(llrb.setts.Int64("valarena.capacity")))
	pcp = humanize.Bytes(uint64(llrb.setts.Int64("valarena.pool.capacity")))
	fmsg = "%v val arena %v blocks over {%v %v} cap %v poolcap %v\n"
	log.Infof(fmsg, llrb.logprefix, vblocks, min, max, cp, pcp)
}

// rotation routines for 2-3-4 algorithm, not used.

func (llrb *LLRB) walkdownrot234(nd *Llrbnode) *Llrbnode {
	if isred(nd.left) && isred(nd.right) {
		llrb.flip(nd)
	}
	return nd
}

func (llrb *LLRB) walkuprot234(nd *Llrbnode) *Llrbnode {
	if isred(nd.right) && !isred(nd.left) {
		nd = llrb.rotateleft(nd)
	}
	if isred(nd.left) && isred(nd.left.left) {
		nd = llrb.rotateright(nd)
	}
	return nd
}
