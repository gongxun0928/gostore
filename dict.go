// +build dict

package storage

import "sort"
import "bytes"
import "fmt"
import "sync/atomic"
import "hash/crc64"

var _ = fmt.Sprintf("dummy")

var crcisotab = crc64.MakeTable(crc64.ISO)

// Dict is a reference data structure, for validation purpose.
type Dict struct {
	id         string
	dict       map[uint64]*dictnode
	sortkeys   []string
	hashks     []uint64
	dead       bool
	snapn      int
	activeiter int64
}

// NewDict create a new golang map for indexing key,value.
func NewDict() *Dict {
	return &Dict{
		id:       "dict",
		dict:     make(map[uint64]*dictnode),
		sortkeys: make([]string, 0, 10000),
	}
}

//---- Index{} interface.

// Count implement Index{} / IndexSnapshot{} interface.
func (d *Dict) Count() int64 {
	return int64(len(d.dict))
}

// Isactive implement Index{} / IndexSnapshot{} interface.
func (d *Dict) Isactive() bool {
	return d.dead == false
}

// RSnapshot implement Index{} interface.
func (d *Dict) RSnapshot(snapch chan IndexSnapshot) error {
	snapch <- d.NewDictSnapshot()
	return nil
}

// Destroy implement Index{} interface.
func (d *Dict) Destroy() error {
	if atomic.LoadInt64(&d.activeiter) > 0 {
		panic("cannot distroy Dict when active iterators are present")
	}

	d.dead = true
	d.dict, d.sortkeys, d.hashks = nil, nil, nil
	return nil
}

// Stats implement Index{} interface.
func (d *Dict) Stats() (map[string]interface{}, error) {
	panic("Index.Stats() not implemented for Dict")
}

// Fullstats implement Index{} interface.
func (d *Dict) Fullstats() (map[string]interface{}, error) {
	panic("Index.Fullstats() not implemented for Dict")
}

// Validate implement Index{} interface.
func (d *Dict) Validate() {
	panic("Index.Validate() not implemented for Dict")
}

// Log implement Index{} interface.
func (d *Dict) Log(involved int, humanize bool) {
	panic("Index.Log() not implemented for Dict")
}

//---- IndexSnapshot{} interface{}

// Id implement IndexSnapshot{} interface.
func (d *Dict) Id() string {
	return d.id
}

// Refer implement IndexSnapshot{} interface.
func (d *Dict) Refer() {
	return
}

// Release implement IndexSnapshot{} interface.
func (d *Dict) Release() {
	d.Destroy()
}

//---- IndexReader{} interface.

// Has implement IndexReader{} interface.
func (d *Dict) Has(key []byte) bool {
	hashv := crc64.Checksum(key, crcisotab)
	_, ok := d.dict[hashv]
	return ok
}

// Get implement IndexReader{} interface.
func (d *Dict) Get(key []byte) Node {
	hashv := crc64.Checksum(key, crcisotab)
	if nd, ok := d.dict[hashv]; ok {
		return nd
	}
	return nil
}

// Min implement IndexReader{} interface.
func (d *Dict) Min() Node {
	if len(d.dict) == 0 {
		return nil
	}
	hashv := d.sorted()[0]
	return d.dict[hashv]
}

// Max implement IndexReader{} interface.
func (d *Dict) Max() Node {
	if len(d.dict) == 0 {
		return nil
	}
	hashks := d.sorted()
	return d.dict[hashks[len(hashks)-1]]
}

// Range implement IndexReader{} interface.
func (d *Dict) Range(lowkey, highkey []byte, incl string, iter RangeCallb) {
	var hashks []uint64

	hashks = d.sorted()

	// parameter rewrite for lookup
	if lowkey != nil && highkey != nil && bytes.Compare(lowkey, highkey) == 0 {
		if incl == "none" {
			return
		} else if incl == "low" || incl == "high" {
			incl = "both"
		}
	}
	if len(hashks) == 0 {
		return
	}

	start, cmp, nd := 0, 1, d.dict[hashks[0]]
	if lowkey != nil {
		if incl == "low" || incl == "both" {
			cmp = 0
		}
		for start = 0; start < len(hashks); start++ {
			nd = d.dict[hashks[start]]
			if bytes.Compare(nd.key, lowkey) >= cmp {
				break
			}
		}
	}

	cmp = 0
	if incl == "high" || incl == "both" {
		cmp = 1
	}
	for ; start < len(hashks); start++ {
		nd = d.dict[hashks[start]]
		if highkey == nil || (bytes.Compare(nd.key, highkey) < cmp) {
			if iter(nd) == false {
				break
			}
			continue
		}
		break
	}
}

// Iterate implement IndexReader{} interface.
func (d *Dict) Iterate(lkey, hkey []byte, incl string, r bool) IndexIterator {
	iter := &dictIterator{
		dict: d.dict, hashks: d.sorted(), activeiter: &d.activeiter, reverse: r,
	}

	// parameter rewrite for lookup
	if lkey != nil && hkey != nil && bytes.Compare(lkey, hkey) == 0 {
		if incl == "none" {
			iter.index = len(iter.hashks)
			if r {
				iter.index = -1
			}
			return iter

		} else if incl == "low" || incl == "high" {
			incl = "both"
		}
	}

	startkey, startincl, endincl, cmp := lkey, "low", "high", 1
	iter.endkey, iter.cmp, iter.index = hkey, 0, 0
	if r {
		startkey, startincl, endincl, cmp = hkey, "high", "low", 0
		iter.endkey, iter.cmp, iter.index = lkey, 1, len(iter.hashks)-1
	}

	if startkey != nil {
		if incl == startincl || incl == "both" {
			cmp = 1 - cmp
		}
		for iter.index = 0; iter.index < len(iter.hashks); iter.index++ {
			nd := d.dict[iter.hashks[iter.index]]
			if bytes.Compare(nd.key, startkey) >= cmp {
				break
			}
		}
		if r {
			iter.index--
		}
	}

	if incl == endincl || incl == "both" {
		iter.cmp = 1 - iter.cmp
	}
	atomic.AddInt64(&d.activeiter, 1)
	return iter
}

//---- IndexWriter{} interface.

// Upsert implement IndexWriter{} interface.
func (d *Dict) Upsert(key, value []byte, callb UpsertCallback) error {
	newnd := newdictnode(key, value)
	hashv := crc64.Checksum(key, crcisotab)
	oldnd, ok := d.dict[hashv]
	if callb != nil {
		if ok == false {
			callb(d, 0, newnd, nil)
		} else {
			callb(d, 0, newnd, oldnd)
		}
	}
	d.dict[hashv] = newnd
	return nil
}

// UpsertMany implement IndexWriter{} interface.
func (d *Dict) UpsertMany(keys, values [][]byte, callb UpsertCallback) error {
	for i, key := range keys {
		var value []byte
		if len(values) > 0 {
			value = values[i]
		}
		newnd := newdictnode(key, value)
		hashv := crc64.Checksum(key, crcisotab)
		oldnd, ok := d.dict[hashv]
		if callb != nil {
			if ok == false {
				callb(d, int64(i), newnd, nil)
			} else {
				callb(d, int64(i), newnd, oldnd)
			}
		}
		d.dict[hashv] = newnd
	}
	return nil
}

// DeleteMin implement IndexWriter{} interface.
func (d *Dict) DeleteMin(callb DeleteCallback) error {
	if len(d.dict) > 0 {
		nd := d.Min()
		d.Delete(nd.Key(), callb)
	}
	return nil
}

// DeleteMax implement IndexWriter{} interface.
func (d *Dict) DeleteMax(callb DeleteCallback) error {
	if len(d.dict) > 0 {
		nd := d.Max()
		d.Delete(nd.Key(), callb)
	}
	return nil
}

// Delete implement IndexWriter{} interface.
func (d *Dict) Delete(key []byte, callb DeleteCallback) error {
	if len(d.dict) > 0 {
		hashv := crc64.Checksum(key, crcisotab)
		deleted, ok := d.dict[hashv]
		if callb != nil {
			if ok == false {
				callb(d, nil)
			} else {
				callb(d, deleted)
			}
		}
		delete(d.dict, hashv)
	}
	return nil
}

func (d *Dict) sorted() []uint64 {
	d.sortkeys, d.hashks = d.sortkeys[:0], d.hashks[:0]
	for _, nd := range d.dict {
		d.sortkeys = append(d.sortkeys, string(nd.key))
	}
	if len(d.sortkeys) > 0 {
		sort.Strings(d.sortkeys)
	}
	for _, key := range d.sortkeys {
		d.hashks = append(d.hashks, crc64.Checksum(str2bytes(key), crcisotab))
	}
	return d.hashks
}
