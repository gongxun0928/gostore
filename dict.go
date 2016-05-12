// +build dict

package storage

import "sort"
import "bytes"
import "fmt"
import "hash/crc64"

var _ = fmt.Sprintf("dummy")

var crcisotab = crc64.MakeTable(crc64.ISO)

// Dict is a reference data structure, for validation purpose.
type Dict struct {
	id       string
	dict     map[uint64]*dictnode
	sortkeys []string
	hashks   []uint64
	dead     bool
	snapn    int
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
	var start int
	var hashks []uint64
	hashks = d.sorted()

	if lowkey == nil {
		start = 0
	} else {
		cmp := 1
		if incl == "low" || incl == "both" {
			cmp = 0
		}
		for start = 0; start < len(hashks); start++ {
			nd := d.dict[hashks[start]]
			if bytes.Compare(nd.key, lowkey) >= cmp {
				break
			}
		}
	}
	if start < len(hashks) {
		cmp := 0
		if incl == "high" || incl == "both" {
			cmp = 1
		}
		for i := start; i < len(hashks); i++ {
			nd := d.dict[hashks[i]]
			if highkey == nil || (bytes.Compare(nd.key, highkey) < cmp) {
				if iter(nd) == false {
					break
				}
				continue
			}
			break
		}
	}
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
