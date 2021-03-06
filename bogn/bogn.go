package bogn

import "io"
import "os"
import "fmt"
import "sort"
import "sync"
import "time"
import "unsafe"
import "reflect"
import "strings"
import "strconv"
import "runtime"
import "math/rand"
import "io/ioutil"
import "sync/atomic"
import "path/filepath"
import "encoding/json"
import "encoding/base64"

import "github.com/bnclabs/gostore/api"
import "github.com/bnclabs/gostore/lib"
import "github.com/bnclabs/gostore/llrb"
import "github.com/bnclabs/gostore/bubt"
import s "github.com/bnclabs/gosettings"
import humanize "github.com/dustin/go-humanize"

// TODO: enable count aggregation across snapshots, with data-structures
// that support LSM it is difficult to maintain accurate count.

// Bogn instance to index key,value pairs.
type Bogn struct {
	// atomic access, 8-byte aligned
	nroutines int64
	dgmstate  int64
	snapspin  int64
	// statistics
	wramplification int64

	name         string
	epoch        time.Time
	snapshot     unsafe.Pointer // *snapshot
	memversions  [3]int
	diskversions [16]int
	finch        chan struct{}
	snaprw       sync.RWMutex
	compactorch  chan []interface{}
	txnmeta

	// bogn settings
	logpath       string
	memstore      string
	diskstore     string
	durable       bool
	dgm           bool
	workingset    bool
	flushratio    float64
	compactratio  float64
	autocommit    time.Duration
	compactperiod time.Duration
	memcapacity   int64
	setts         s.Settings
	logprefix     string
}

// PurgeIndex will purge all the disk level snapshots for index `name`
// founder under `diskpaths`.
func PurgeIndex(name, logpath, diskstore string, diskpaths []string) {
	bogn := &Bogn{name: name}
	bogn.logprefix = fmt.Sprintf("BOGN [%v]", name)
	bogn.destroydisksnaps("purge", logpath, diskstore, diskpaths)
	return
}

// CompactIndex will remove older versions of disk level snapshots and
// if merge is true, will merge all disk-levels into single level.
func CompactIndex(name, diskstore string, diskpaths []string, merge bool) {
	bogn := &Bogn{name: name, diskstore: diskstore, snapshot: nil}
	bogn.logprefix = fmt.Sprintf("BOGN [%v]", name)
	bogn.compactdisksnaps("compactindex", diskstore, diskpaths, merge)
	return
}

// New create a new bogn instance.
func New(name string, setts s.Settings) (*Bogn, error) {
	bogn := (&Bogn{
		name:      name,
		logprefix: fmt.Sprintf("BOGN [%v]", name),
	}).readsettings(setts)
	bogn.inittxns()
	bogn.epoch = time.Now()
	if err := bogn.makepaths(setts); err != nil {
		return nil, err
	}
	bogn.finch = make(chan struct{})

	startedat := bogn.epoch.Format(time.RFC3339Nano)
	infof("%v boot: starting epoch@%v ...", bogn.logprefix, startedat)

	merge := false
	CompactIndex(bogn.name, bogn.diskstore, bogn.getdiskpaths(), merge)

	disks, err := bogn.opendisksnaps(setts)
	if err != nil {
		bogn.Close()
		return nil, err
	}
	// NOTE: If settings have changed in between a re-boot from disk,
	// user should use a migration tool to move disk snapshots
	// from older settings to new settings.
	lastseqno := bogn.loaddisksettings(disks[:])

	mw := bogn.warmupfromdisk(disks[:])

	head, err := opensnapshot(bogn, mw, disks, lastseqno)
	if err != nil {
		bogn.Close()
		return nil, err
	}
	head.refer()
	bogn.setheadsnapshot(head)

	return bogn, nil
}

// IMPORTANT: when ever this functin is updated, please update
// settingsfromdisk(), loaddisksettings(), settingstodisk() and
// validatesettings().
func (bogn *Bogn) readsettings(setts s.Settings) *Bogn {
	bogn.logpath = setts.String("logpath")
	bogn.memstore = setts.String("memstore")
	bogn.diskstore = setts.String("diskstore")
	bogn.durable = setts.Bool("durable")
	bogn.dgm = setts.Bool("dgm")
	bogn.workingset = setts.Bool("workingset")
	bogn.flushratio = setts.Float64("flushratio")
	bogn.compactratio = setts.Float64("compactratio")
	bogn.autocommit = time.Duration(setts.Int64("autocommit"))
	bogn.autocommit *= time.Second
	bogn.compactperiod = time.Duration(setts.Int64("compactperiod"))
	bogn.compactperiod *= time.Second
	bogn.setts = setts

	atomic.StoreInt64(&bogn.dgmstate, 0)
	if bogn.dgm {
		atomic.StoreInt64(&bogn.dgmstate, 1)
	}

	// validate
	switch bogn.memstore {
	case "llrb", "mvcc":
	default:
		panic(fmt.Errorf("invalid memstore %q", bogn.memstore))
	}
	switch bogn.diskstore {
	case "bubt":
	default:
		panic(fmt.Errorf("invalid diskstore %q", bogn.diskstore))
	}

	// pick a logpath, if not supplied, from bubt-diskpaths.
	if bogn.durable {
		if len(bogn.logpath) == 0 {
			switch bogn.diskstore {
			case "bubt":
				diskpaths := bogn.getdiskpaths()
				if len(diskpaths) == 0 {
					panic(fmt.Errorf("missing bubt `diskpaths` settings"))
				}
				bogn.logpath = diskpaths[rand.Intn(10000)%len(diskpaths)]

			default:
				panic(fmt.Errorf("invalid diskstore %q", bogn.diskstore))
			}
		}
		if len(bogn.logpath) == 0 {
			panic("unable to pick/locate a logdir")
		}
		infof("%v boot: with logpath %q", bogn.logprefix, bogn.logpath)
	}

	return bogn.readmemsettings(setts)
}

func (bogn *Bogn) readmemsettings(setts s.Settings) *Bogn {
	switch bogn.memstore {
	case "llrb", "mvcc":
		llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
		bogn.memcapacity = llrbsetts.Int64("memcapacity")
	}
	return bogn
}

func (bogn *Bogn) settingstodisk() s.Settings {
	memversions := bogn.memversions
	diskversions := bogn.diskversions
	setts := s.Settings{
		"logpath":       bogn.logpath,
		"memstore":      bogn.memstore,
		"diskstore":     bogn.diskstore,
		"workingset":    bogn.workingset,
		"flushratio":    bogn.flushratio,
		"compactratio":  bogn.compactratio,
		"autocommit":    bogn.autocommit,
		"compactperiod": bogn.compactperiod,
		"memversions":   memversions,
		"diskversions":  diskversions,
	}
	llrbsetts := bogn.setts.Section("llrb.")
	bubtsetts := bogn.setts.Section("bubt.")
	setts = (s.Settings{}).Mixin(setts, llrbsetts, bubtsetts)
	return setts
}

// priority of settings.
// bogn-settings - settings from application passed to New() take priority.
// llrb-settings - settings from application passed to New() take priority.
// bubt-settings - settings from ndisk take priority.
func (bogn *Bogn) loaddisksettings(disks []api.Index) (seqno uint64) {
	alldisks := []api.Index{}
	for i, disk := range disks {
		if disk == nil {
			continue
		}
		level, _, _ := bogn.path2level(disk.ID())
		if level != i {
			panic(fmt.Errorf("expected level %v, got %v", i, level))
		}
		alldisks = append(alldisks, disk)
	}
	if len(alldisks) > 0 {
		disksetts := bogn.settingsfromdisk(alldisks[0])
		bogn.memversions = disksetts["memversions"].([3]int)
		bogn.diskversions = disksetts["diskversions"].([16]int)
		bogn.logpath = disksetts.String("logpath")
		bogn.validatesettings(disksetts)
		return bogn.getdiskseqno(alldisks[0])
	}
	return 0
}

func (bogn *Bogn) validatesettings(disksetts s.Settings) {
	setts := bogn.setts
	if memstore := disksetts.String("memstore"); memstore != bogn.memstore {
		fmsg := "found memstore:%q on disk, expected %q"
		panic(fmt.Errorf(fmsg, memstore, bogn.memstore))
	}
	diskstore := disksetts.String("diskstore")
	if diskstore != bogn.diskstore {
		fmsg := "found diskstore:%q on disk, expected %q"
		panic(fmt.Errorf(fmsg, diskstore, bogn.diskstore))
	}
	if bogn.durable {
		if logpath := disksetts.String("logpath"); logpath != bogn.logpath {
			fmsg := "found logpath:%q on disk, expected %q"
			panic(fmt.Errorf(fmsg, logpath, bogn.logpath))
		}
	}

	// bubt settings
	diskpaths1 := disksetts.Strings("bubt.diskpaths")
	sort.Strings(diskpaths1)
	diskpaths2 := setts.Strings("bubt.diskpaths")
	sort.Strings(diskpaths2)
	if reflect.DeepEqual(diskpaths1, diskpaths2) == false {
		fmsg := "found diskpaths:%v on disk, expected %v"
		panic(fmt.Errorf(fmsg, diskpaths1, diskpaths2))
	}
	msize1 := disksetts.Int64("bubt.mblocksize")
	msize2 := setts.Int64("bubt.mblocksize")
	if msize1 != msize2 {
		fmsg := "found mblocksize:%v on disk, expected %v"
		panic(fmt.Errorf(fmsg, msize1, msize2))
	}
	zsize1 := disksetts.Int64("bubt.zblocksize")
	zsize2 := setts.Int64("bubt.zblocksize")
	if zsize1 != zsize2 {
		fmsg := "found zsize:%v on disk, expected %v"
		panic(fmt.Errorf(fmsg, zsize1, zsize2))
	}
	vsize1 := disksetts.Int64("bubt.vblocksize")
	vsize2 := setts.Int64("bubt.vblocksize")
	if vsize1 != vsize2 {
		fmsg := "found vsize:%v on disk, expected %v"
		panic(fmt.Errorf(fmsg, vsize1, vsize2))
	}
	mmap1, mmap2 := disksetts.Bool("bubt.mmap"), disksetts.Bool("bubt.mmap")
	if mmap1 != mmap2 {
		fmsg := "found mmap:%v on disk, expected %v"
		panic(fmt.Errorf(fmsg, mmap1, mmap2))
	}
}

func (bogn *Bogn) settingsfromdisk(disk api.Index) s.Settings {
	switch d := disk.(type) {
	case *bubt.Snapshot:
		metadata := s.Settings(bogn.diskmetadata(d))
		return metadata
	}
	panic("unreachable code")
}

// create a new in-memory snapshot from latest disk snapshot, if
// there is not enough memory to hold the latest disk snapshot
// return nil.
func (bogn *Bogn) warmupfromdisk(disks []api.Index) api.Index {
	var ndisk api.Index

	for _, ndisk = range disks {
		if ndisk != nil {
			break
		}
	}
	if ndisk == nil {
		return nil
	}

	var memcapacity int64
	payload, entries := bogn.indexpayload(ndisk), bogn.indexcount(ndisk)

	switch bogn.memstore {
	case "llrb":
		llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
		memcapacity = llrbsetts.Int64("memcapacity")
		nodesize := int64(unsafe.Sizeof(llrb.Llrbnode{})) - 8
		if expected := (nodesize * 2) * entries; expected < memcapacity {
			return bogn.llrbfromdisk(ndisk, entries, payload)
		} else {
			bogn.dgmstate = 1
		}

	case "mvcc":
		llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
		memcapacity = llrbsetts.Int64("memcapacity")
		nodesize := int64(unsafe.Sizeof(llrb.Llrbnode{})) - 8
		if expected := (nodesize * 2) * entries; expected < memcapacity {
			return bogn.mvccfromdisk(ndisk, entries, payload)
		} else {
			bogn.dgmstate = 1
		}

	default:
		panic("unreachable code")
	}

	fmsg := "%v warmup: memory capacity %v too small for %v, %v entries"
	arg1 := humanize.Bytes(uint64(memcapacity))
	arg2 := humanize.Bytes(uint64(payload))
	infof(fmsg, bogn.logprefix, arg1, arg2, entries)
	return nil
}

func (bogn *Bogn) llrbfromdisk(
	ndisk api.Index, entries, payload int64) api.Index {

	now := time.Now()

	bogn.memversions[0]++
	iter, seqno := ndisk.Scan(), bogn.getdiskseqno(ndisk)
	name := bogn.memlevelname("mw", bogn.memversions[0])
	llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
	mw := llrb.LoadLLRB(name, llrbsetts, iter)
	mw.Setseqno(seqno)
	iter(true /*fin*/)

	fmsg := "%v warmup: LLRB %v (%v) %v entries -> %v in %v"
	arg1 := humanize.Bytes(uint64(payload))
	took := time.Since(now).Round(time.Second)
	infof(fmsg, bogn.logprefix, ndisk.ID(), arg1, entries, mw.ID(), took)

	return mw
}

func (bogn *Bogn) mvccfromdisk(
	ndisk api.Index, entries, payload int64) api.Index {

	now := time.Now()

	bogn.memversions[0]++
	iter, seqno := ndisk.Scan(), bogn.getdiskseqno(ndisk)
	name := bogn.memlevelname("mw", bogn.memversions[0])
	llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
	mw := llrb.LoadMVCC(name, llrbsetts, iter)
	mw.Setseqno(seqno)
	iter(true /*fin*/)

	fmsg := "%v warmup: MVCC %v (%v) %v entries -> %v in %v"
	arg1 := humanize.Bytes(uint64(payload))
	took := time.Since(now).Round(time.Second)
	infof(fmsg, bogn.logprefix, ndisk.ID(), arg1, entries, mw.ID(), took)

	return mw
}

// Start bogn service. Typically bogn instances are created and
// started as:
//   inst := NewBogn("storage", setts).Start()
func (bogn *Bogn) Start() *Bogn {
	bogn.compactorch = make(chan []interface{}, 128)
	go purger(bogn)
	go compactor(bogn, bogn.compactorch)

	// wait until all routines have started.
	for atomic.LoadInt64(&bogn.nroutines) < 2 {
		runtime.Gosched()
	}
	return bogn
}

func (bogn *Bogn) makepaths(setts s.Settings) error {
	var diskpaths []string

	switch bogn.diskstore {
	case "bubt":
		diskpaths = bogn.getdiskpaths()
	default:
		panic("impossible situation")
	}

	for _, path := range diskpaths {
		if err := os.MkdirAll(path, 0775); err != nil {
			errorf("%v %v", bogn.logprefix, err)
			return err
		}
	}

	// create logpath, please do this after creating `diskpaths`
	// because logpath might be one of the diskpaths.
	if bogn.durable {
		logdir := bogn.logdir(bogn.logpath)
		if err := os.MkdirAll(logdir, 0775); err != nil {
			errorf("%v %v", bogn.logprefix, err)
			return err
		}
	}
	return nil
}

func (bogn *Bogn) currsnapshot() *snapshot {
	return (*snapshot)(atomic.LoadPointer(&bogn.snapshot))
}

func (bogn *Bogn) setheadsnapshot(snapshot *snapshot) {
	atomic.StorePointer(&bogn.snapshot, unsafe.Pointer(snapshot))
}

func (bogn *Bogn) latestsnapshot() *snapshot {
	for {
		snap := bogn.currsnapshot()
		if snap == nil {
			return nil
		}
		snap.refer()
		if snap.istrypurge() == false {
			return snap
		}
		snap.release()
		runtime.Gosched()
	}
	panic("unreachable code")
}

var writelatch int64 = 0x10000
var writelock int64 = 0x4000000000000000

func (bogn *Bogn) snaprlock() {
	for {
		expected := atomic.LoadInt64(&bogn.snapspin)
		if (expected & 0xFFFF0000) == 0 { // no writelatches
			desired := expected + 1
			if atomic.CompareAndSwapInt64(&bogn.snapspin, expected, desired) {
				return
			}
		}
		runtime.Gosched()
	}
}

func (bogn *Bogn) snaprunlock() {
	for {
		expected := atomic.LoadInt64(&bogn.snapspin)
		desired := expected - 1
		if atomic.CompareAndSwapInt64(&bogn.snapspin, expected, desired) {
			return
		}
		runtime.Gosched()
	}
}

func (bogn *Bogn) snaplock() {
	addlatch := func() {
		for {
			expected := atomic.LoadInt64(&bogn.snapspin)
			desired := expected + writelatch
			if atomic.CompareAndSwapInt64(&bogn.snapspin, expected, desired) {
				return
			}
			runtime.Gosched()
		}
	}

	addlatch()
	for {
		expected := atomic.LoadInt64(&bogn.snapspin)
		ok1 := (expected & 0xFFFF) == 0    // no readers
		ok2 := (expected & writelock) == 0 // no writers
		if ok1 && ok2 {
			desired := expected | writelock
			if atomic.CompareAndSwapInt64(&bogn.snapspin, expected, desired) {
				return
			}
		}
		runtime.Gosched()
	}
}

func (bogn *Bogn) snapunlock() {
	for {
		expected := atomic.LoadInt64(&bogn.snapspin)
		desired := expected & (^writelock) // release the lock
		desired -= writelatch
		if atomic.CompareAndSwapInt64(&bogn.snapspin, expected, desired) {
			return
		}
		runtime.Gosched()
	}
}

func (bogn *Bogn) mwmetadata(
	seqno uint64, flushunix string, appdata []byte,
	settstodisk s.Settings) []byte {

	if len(flushunix) == 0 {
		flushunix = fmt.Sprintf(`"%v"`, uint64(time.Now().Unix()))
	}
	appdatastr := base64.StdEncoding.EncodeToString(appdata)
	metadata := map[string]interface{}{
		"seqno":     fmt.Sprintf(`"%v"`, seqno),
		"flushunix": flushunix,
		"appdata":   appdatastr,
	}
	setts := (s.Settings{}).Mixin(settstodisk, metadata)
	setts = setts.AddPrefix("bogn.")
	data, err := json.Marshal(setts)
	if err != nil {
		panic(err)
	}
	return data
}

func (bogn *Bogn) flushelapsed() bool {
	snap := bogn.currsnapshot()
	if snap == nil {
		return false
	}
	_, disk := snap.latestlevel()
	if disk == nil {
		return int64(time.Since(bogn.epoch)) > int64(bogn.autocommit)
	}
	metadata := bogn.diskmetadata(disk)
	x, _ := strconv.Atoi(strings.Trim(metadata["flushunix"].(string), `"`))
	return time.Now().Sub(time.Unix(int64(x), 0)) > bogn.autocommit
}

// cdisks is a list of disk snapshots being compacted.
func (bogn *Bogn) pickflushdisk(
	cdisks []api.Index) (fdisks []api.Index, nlevel int, what string) {

	var ok bool

	if fdisks, nlevel, ok = bogn.pickflushdisk1(cdisks); ok {
		return fdisks, nlevel, "flush.fresh"
	} else if fdisks, nlevel, ok = bogn.pickflushdisk2(cdisks); ok {
		return fdisks, nlevel, "flush.aggressive"
	} else if fdisks, nlevel, ok = bogn.pickflushdisk3(cdisks); ok {
		return fdisks, nlevel, "flush.fallback"
	} else if fdisks, nlevel, ok = bogn.pickflushdisk4(cdisks); ok {
		return fdisks, nlevel, "flush.merge"
	}
	panic("unreachable code")
}

// first time flush
func (bogn *Bogn) pickflushdisk1(
	cdisks []api.Index) (fdisks []api.Index, nlevel int, ok bool) {

	snap := bogn.currsnapshot()
	latestlevel, _ := snap.latestlevel()
	if latestlevel < 0 && len(cdisks) > 0 {
		panic("impossible situation")

	} else if latestlevel < 0 { // first time flush.
		return nil, len(snap.disks) - 1, true
	}
	return nil, -1, false
}

// if all of the allowed-snapshot levels are exhausted then flush by
// merging all snapshot levels.
func (bogn *Bogn) pickflushdisk2(
	cdisks []api.Index) (fdisks []api.Index, nlevel int, ok bool) {

	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})
	if len(disks) == len(snap.disks) {
		till := 16
		if len(cdisks) > 0 {
			till, _, _ = bogn.path2level(cdisks[0].ID())
		}
		fdisks := []api.Index{}
		for _, disk := range disks {
			if level, _, _ := bogn.path2level(disk.ID()); level >= till {
				break
			}
			fdisks = append(fdisks, disk)
		}
		if len(fdisks) == 0 { // all of them are being compacted
			return nil, -1, true
		}
		level, _, _ := bogn.path2level(fdisks[len(fdisks)-1].ID())
		return fdisks, snap.nextbutlevel(level), true
	}
	return nil, -1, false
}

// fallback by one level and flush without merge
func (bogn *Bogn) pickflushdisk3(
	cdisks []api.Index) (fdisks []api.Index, nlevel int, ok bool) {

	snap := bogn.currsnapshot()
	latestlevel, latestdisk := snap.latestlevel()
	if latestlevel <= 0 { // handled by 1 & 2.
		panic("impossible situation")
	}
	if len(cdisks) > 0 {
		level0, _, _ := bogn.path2level(cdisks[0].ID())
		if latestlevel > level0 {
			panic("impossible situation")
		} else if latestlevel == level0 {
			return nil, latestlevel - 1, true // fallback by one level.
		}
	}

	payload := float64(bogn.indexpayload(latestdisk))
	if (float64(snap.memheap()) / payload) < bogn.flushratio {
		return nil, latestlevel - 1, true // fallback by one level
	}

	return nil, -1, false
}

// pick the latest disk snapshot and flush with merge.
func (bogn *Bogn) pickflushdisk4(
	cdisks []api.Index) (fdisks []api.Index, nlevel int, ok bool) {

	snap := bogn.currsnapshot()
	latestlevel, latestdisk := snap.latestlevel()
	return []api.Index{latestdisk}, snap.nextbutlevel(latestlevel), true
}

func (bogn *Bogn) pickcompactdisks(tombstonepurge bool) (
	disks []api.Index, nextlevel int, what string) {

	var ok bool

	// "offlinemerge", "persist"
	if disks, nextlevel, ok = bogn.pickcompactdisks1(tombstonepurge); ok {
		return disks, nextlevel, "compact.tombstonepurge"
	} else if disks, nextlevel, ok = bogn.pickcompactdisks2(); ok {
		return disks, nextlevel, "compact.none"
	} else if disks, nextlevel, ok = bogn.pickcompactdisks3(); ok {
		return disks, nextlevel, "compact.aggressive"
	} else if disks, nextlevel, ok = bogn.pickcompactdisks4(); ok {
		return disks, nextlevel, "compact.ratio"
	} else if disks, nextlevel, ok = bogn.pickcompactdisks5(); ok {
		return disks, nextlevel, "compact.period"
	} else if disks, nextlevel, ok = bogn.pickcompactdisks6(); ok {
		return disks, nextlevel, "compact.self"
	}
	return nil, -1, "none"
}

// tombstone purge for the last level
func (bogn *Bogn) pickcompactdisks1(tombstonepurge bool) (
	cdisks []api.Index, nextlevel int, ok bool) {

	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})

	if tombstonepurge == false || len(disks) == 0 {
		return nil, -1, false
	}

	disk := disks[len(disks)-1]
	level, _, _ := bogn.path2level(disk.ID())
	if level != len(snap.disks)-1 {
		panic("impossible situation")
	}
	return []api.Index{disk}, snap.nextbutlevel(level), true
}

// no compaction: there is only zero or one disk level
func (bogn *Bogn) pickcompactdisks2() (
	cdisks []api.Index, nextlevel int, ok bool) {

	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})
	if len(disks) <= 1 {
		return nil, -1, true
	}
	return nil, -1, false
}

// aggressive compaction, if number of levels is more than 3 then
// compact without checking for compactratio or compactperiod.
func (bogn *Bogn) pickcompactdisks3() (
	cdisks []api.Index, nextlevel int, ok bool) {

	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})
	if len(disks) > 3 {
		// leave the first level for flusher logic, and leave the
		// last level since it might be too big !!
		cdisks = disks[1 : len(disks)-1]
		level, _, _ := bogn.path2level(cdisks[len(cdisks)-1].ID())
		return cdisks, snap.nextbutlevel(level), true
	}
	return nil, -1, false
}

// check whether ratio between two snapshot's payload exceeds compactratio.
func (bogn *Bogn) pickcompactdisks4() (
	cdisks []api.Index, nextlevel int, ok bool) {

	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})
	for i := 0; i < len(disks)-1; i++ {
		disk0, disk1 := disks[i], disks[i+1]
		payload0 := float64(bogn.indexpayload(disk0))
		payload1 := float64(bogn.indexpayload(disk1))
		if (payload0 / payload1) > bogn.compactratio {
			level1, _, _ := bogn.path2level(disk1.ID())
			cdisks := []api.Index{disk0, disk1}
			return cdisks, snap.nextbutlevel(level1), true
		}
	}
	return nil, -1, false
}

// check whether disk's lifetime exceeds compact period.
func (bogn *Bogn) pickcompactdisks5() (
	cdisks []api.Index, nextlevel int, ok bool) {

	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})
	for i := 0; i < len(disks)-1; i++ {
		mdata := bogn.diskmetadata(disks[i])
		x, _ := strconv.Atoi(strings.Trim(mdata["flushunix"].(string), `"`))
		if time.Now().Sub(time.Unix(int64(x), 0)) > bogn.compactperiod {
			level, _, _ := bogn.path2level(disks[i].ID())
			if cdisks := disks[i:]; len(cdisks) > 1 {
				return cdisks, snap.nextbutlevel(level), true
			}
		}
	}
	return nil, -1, false
}

func (bogn *Bogn) pickcompactdisks6() (
	cdisks []api.Index, nextlevel int, ok bool) {

	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})
	disk := disks[len(disks)-1]
	level, _, _ := bogn.path2level(disk.ID())
	if level != len(snap.disks)-1 {
		panic("impossible situation")
	}
	payload := float64(bogn.indexpayload(disk))
	footprint := float64(bogn.indexfootprint(disk))
	if (payload / footprint) < 0.25 { // TODO: no magic number
		return []api.Index{disk}, level, true
	}
	return nil, -1, false
}

func (bogn *Bogn) pickwindupdisk() (disk api.Index, nlevel int) {
	snap := bogn.currsnapshot()

	latestlevel, latestdisk := snap.latestlevel()
	if latestlevel < 0 { // first time flush.
		return nil, len(snap.disks) - 1

	} else if latestlevel > 0 {
		payload := float64(bogn.indexpayload(latestdisk))
		if (float64(snap.memheap()) / payload) < bogn.flushratio {
			return nil, latestlevel - 1
		}
	}
	return latestdisk, snap.nextbutlevel(latestlevel)
}

func (bogn *Bogn) levelname(level, version int, sha string) string {
	return fmt.Sprintf("%v-%v-%v-%v", bogn.name, level, version, sha)
}

func (bogn *Bogn) memlevelname(level string, version int) string {
	// level can be mw or mr or mc.
	return fmt.Sprintf("%v-%v-%v", bogn.name, level, version)
}

func (bogn *Bogn) logdir(logpath string) string {
	if len(logpath) == 0 && len(bogn.logpath) > 0 {
		logpath = bogn.logpath
	} else if len(bogn.logpath) == 0 {
		return ""
	}
	dirname := fmt.Sprintf("bogn-%v-logs", bogn.name)
	return filepath.Join(logpath, dirname)
}

func (bogn *Bogn) path2level(dirname string) (level, ver int, uuid string) {
	var err error

	parts := strings.Split(dirname, "-")
	if len(parts) == 4 && parts[0] == bogn.name {
		if level, err = strconv.Atoi(parts[1]); err != nil {
			return -1, -1, ""
		}
		if ver, err = strconv.Atoi(parts[2]); err != nil {
			return -1, -1, ""
		}
		return level, ver, parts[3]
	}
	return -1, -1, ""
}

func (bogn *Bogn) isclosed() bool {
	select {
	case <-bogn.finch:
		return true
	default:
	}
	return false
}

//---- Exported Control methods

// ID is same as the name of the instance used when creating it.
func (bogn *Bogn) ID() string {
	return bogn.name
}

// Getseqno return current mutation seqno on write path.
func (bogn *Bogn) Getseqno() uint64 {
	return bogn.currsnapshot().mwseqno()
}

// Disksnapshots return a iterator to iterate on disk snapshots. Until
// the iterator is closed, by calling diskiterator(true /*fin*/), all
// write operations will be blocked. Caller can iterate until a nil is
// return for api.Disksnapshot
func (bogn *Bogn) Disksnapshots() func(fin bool) api.Disksnapshot {
	done := false
	bogn.snaplock()
	snap := bogn.currsnapshot()
	disks := snap.disklevels([]api.Index{})

	return func(fin bool) api.Disksnapshot {
		if done {
			return nil

		} else if len(disks) == 0 || fin {
			bogn.snapunlock()
			done, disks = true, nil
			return nil
		}
		ds := disksnap{bogn: bogn, disk: disks[0]}
		disks = disks[1:]
		return &ds
	}
}

// BeginTxn starts a read-write transaction. All transactions should either
// be committed or aborted. If transactions are not released for long time
// it might increase the memory pressure on the system. Concurrent
// transactions are allowed, and serialized internally.
func (bogn *Bogn) BeginTxn(id uint64) api.Transactor {
	bogn.snaprlock()
	if snap := bogn.latestsnapshot(); snap != nil {
		txn := bogn.gettxn(id, bogn, snap)
		return txn
	}
	return nil
}

func (bogn *Bogn) commit(txn *Txn) (err error) {
	txn.snap.release()
	bogn.puttxn(txn)

	bogn.snaprunlock()
	return err
}

func (bogn *Bogn) aborttxn(txn *Txn) error {
	txn.snap.release()
	bogn.puttxn(txn)

	bogn.snaprunlock()
	return nil
}

// View starts a read-only transaction. Other than that it is similar
// to BeginTxn. All view transactions should be aborted.
func (bogn *Bogn) View(id uint64) api.Transactor {
	bogn.snaprlock()
	if snap := bogn.latestsnapshot(); snap != nil {
		view := bogn.getview(id, bogn, snap)
		return view
	}
	return nil
}

func (bogn *Bogn) abortview(view *View) error {
	view.snap.release()
	bogn.putview(view)

	bogn.snaprunlock()
	return nil
}

// TombstonePurge call will remove all entries marked as deleted from
// the oldest and top-most disk level, provided the highest seqno stored
// in that level is less that `seqno`.
func (bogn *Bogn) TombstonePurge() {
	tombstonepurge(bogn)
}

// Commit will trigger a memory to disk flush and/or disk compaction.
// Applications can supply appdata that will be stored as part of the
// lastest snapshot.
func (bogn *Bogn) Commit(appdata []byte) {
	postcommit(bogn, appdata)
}

// Log vital statistics for all active bogn levels.
func (bogn *Bogn) Log() {
	bogn.snaprlock()
	defer bogn.snaprunlock()

	snap := bogn.latestsnapshot()
	if snap.mw != nil {
		bogn.logstore(snap.mw)
	}
	if snap.mr != nil {
		bogn.logstore(snap.mw)
	}
	if snap.mc != nil {
		bogn.logstore(snap.mw)
	}
	for _, disk := range snap.disklevels([]api.Index{}) {
		bogn.logstore(disk)
	}
	snap.release()
}

// Validate active bogn levels.
func (bogn *Bogn) Validate() {
	bogn.snaprlock()
	defer bogn.snaprunlock()

	// standard validation
	snap := bogn.latestsnapshot()
	disks := snap.disklevels([]api.Index{})
	seqno, endseqno := uint64(0), uint64(0)
	for i := len(disks) - 1; i >= 0; i-- {
		disk := disks[i]
		if disk != nil {
			endseqno = bogn.validatedisklevel(disk, seqno)
			fmsg := "%v validate: disk %v (after seqno %v to %v) ... ok"
			infof(fmsg, bogn.logprefix, disk.ID(), seqno, endseqno)
			seqno = endseqno
		}
	}

	if snap.mw != nil {
		bogn.validatestore(snap.mw)
	}
	if snap.mc != nil {
		bogn.validatestore(snap.mc)
	}

	snap.release()
}

func (bogn *Bogn) validatedisklevel(
	index api.Index, minseqno uint64) (maxseqno uint64) {

	var count int64

	switch idx := index.(type) {
	case *bubt.Snapshot:
		maxseqno, count = idx.Getseqno(), idx.Count()
		idx.Validate()
	}
	fmsg := "%v validate: found %v entries in %v"
	infof(fmsg, bogn.logprefix, count, index.ID())
	return maxseqno
}

// Close this instance, no calls allowed after Close.
func (bogn *Bogn) Close() {
	if bogn.autocommit == 0 {
		if snap := bogn.currsnapshot(); snap.isdirty() {
			panic("commit before close")
		}
	}

	compactorclose(bogn)
	close(bogn.finch)

	for atomic.LoadInt64(&bogn.nroutines) > 0 {
		time.Sleep(10 * time.Millisecond)
	}

	bogn.logstatistics("close")

	// check whether all mutations are flushed to disk.
	snap := bogn.currsnapshot()
	mwseqno, disks := bogn.indexseqno(snap.mw), snap.disklevels([]api.Index{})
	if len(disks) > 0 && mwseqno > 0 {
		disk := disks[0]
		if diskseqno := bogn.getdiskseqno(disk); diskseqno != mwseqno {
			fmsg := "diskseqno(%v) != mwseqno(%v)"
			panic(fmt.Errorf(fmsg, diskseqno, mwseqno))
		}
	}

	// close disk snapshots.
	for _, disk := range snap.disks {
		if disk != nil {
			infof("%v closing disk snapshot %v", bogn.logprefix, disk.ID())
			disk.Close()
		}
	}

	// clear up the current snapshots and all the entire list.
	snap.addtopurge(snap.mw, snap.mr, snap.mc)
	for purgesnapshot(snap) == false {
		time.Sleep(10 * time.Millisecond)
		snap = bogn.currsnapshot()
	}
	bogn.setheadsnapshot(nil)

	infof("%v closed ...", bogn.logprefix)
}

// Destroy the disk snapshots of this instance, no calls allowed after Destroy.
func (bogn *Bogn) Destroy() {
	diskpaths := bogn.getdiskpaths()
	bogn.destroydisksnaps("destory", bogn.logpath, bogn.diskstore, diskpaths)
	infof("%v destroyed ...", bogn.logprefix)
	return
}

//---- Exported read methods

// Get value for key, if value argument points to valid buffer it will, be
// used to copy the entry's value. Also returns entry's cas, whether entry
// is marked as deleted by LSM. If ok is false, then key is not found.
func (bogn *Bogn) Get(key, value []byte) (v []byte, cas uint64, del, ok bool) {
	snap := bogn.latestsnapshot()
	if snap.yget != nil {
		v, cas, del, ok = snap.yget(key, value)
	}
	snap.release()
	return
}

// Scan return a full table iterator, if iteration is stopped before
// reaching end of table (io.EOF), application should call iterator
// with fin as true. EG: iter(true)
func (bogn *Bogn) Scan() api.Iterator {
	var key, value []byte
	var seqno uint64
	var del bool
	var err error

	snap := bogn.latestsnapshot()
	iter := snap.iterator()
	return func(fin bool) ([]byte, []byte, uint64, bool, error) {
		if err == io.EOF {
			return nil, nil, 0, false, err

		} else if iter == nil {
			err = io.EOF
			snap.release()
			return nil, nil, 0, false, err

		} else if fin {
			iter(fin) // close all underlying iterations.
			err = io.EOF
			snap.release()
			return nil, nil, 0, false, err
		}
		if key, value, seqno, del, err = iter(fin); err == io.EOF {
			iter(fin)
			snap.release()
		}
		return key, value, seqno, del, err
	}
}

// ScanEntries is not supported by Bogn.
func (bogn *Bogn) ScanEntries() api.EntryIterator {
	panic("unsupported API")
}

//---- Exported write methods

// Set a key, value pair in the index, if key is already present, its value
// will be over-written. Make sure key is not nil. Return old value if
// oldvalue points to valid buffer.
func (bogn *Bogn) Set(key, value, oldvalue []byte) (ov []byte, cas uint64) {
	bogn.snaprlock()
	ov, cas = bogn.currsnapshot().set(key, value, oldvalue)
	bogn.snaprunlock()
	return ov, cas
}

// SetCAS a key, value pair in the index, if CAS is ZERO then key should
// not be present in the index, otherwise existing CAS should match the
// supplied CAS. Value will be over-written. Make sure key is not nil.
// Return old value if oldvalue points to valid buffer.
func (bogn *Bogn) SetCAS(
	key, value, oldvalue []byte, cas uint64) ([]byte, uint64, error) {

	ov, rccas, err, ok := bogn.setcasMem(key, value, oldvalue, cas)
	if ok {
		return ov, rccas, err
	}

	rccas = 0

	txn := bogn.BeginTxn(0xABBA)
	_, gcas, deleted, ok := txn.Get(key, nil)
	ok1 := (ok && deleted == false) && gcas != cas
	ok2 := (ok == false || deleted) && cas != 0
	if ok1 || ok2 {
		txn.Abort()
		return oldvalue, 0, api.ErrorInvalidCAS
	}
	ov = txn.Set(key, value, oldvalue)
	err = txn.Commit()
	return ov, rccas, err
}

func (bogn *Bogn) setcasMem(
	key, value, oldvalue []byte, cas uint64) ([]byte, uint64, error, bool) {

	var ov []byte
	var rccas uint64
	var err error

	ok := false

	bogn.snaprlock()
	if atomic.LoadInt64(&bogn.dgmstate) == 0 {
		ov, rccas, err = bogn.currsnapshot().setCAS(key, value, oldvalue, cas)
		ok = true
	}
	bogn.snaprunlock()
	return ov, rccas, err, ok
}

// Delete key from index. Key should not be nil, if key found return its
// value. If lsm is true, then don't delete the node instead mark the node
// as deleted. Again, if lsm is true but key is not found in index, a new
// entry will inserted.
func (bogn *Bogn) Delete(key, oldvalue []byte, lsm bool) ([]byte, uint64) {
	bogn.snaprlock()
	if atomic.LoadInt64(&bogn.dgmstate) == 1 { // auto-enable lsm in dgm
		lsm = true
	}
	ov, cas := bogn.currsnapshot().delete(key, oldvalue, lsm)
	bogn.snaprunlock()
	return ov, cas
}

//---- local methods

func (bogn *Bogn) newmemstore(
	logprefix, level string, seqno uint64) (api.Index, error) {

	var name string

	switch level {
	case "mw":
		bogn.memversions[0]++
		name = bogn.memlevelname(level, bogn.memversions[0])
	case "mc":
		bogn.memversions[2]++
		name = bogn.memlevelname(level, bogn.memversions[2])
	}

	switch bogn.memstore {
	case "llrb":
		llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
		index := llrb.NewLLRB(name, llrbsetts)
		index.Setseqno(seqno)
		infof("%v %v: new llrb store %q", bogn.logprefix, logprefix, name)
		return index, nil

	case "mvcc":
		llrbsetts := bogn.setts.Section("llrb.").Trim("llrb.")
		index := llrb.NewMVCC(name, llrbsetts)
		index.Setseqno(seqno)
		infof("%v %v: new mvcc store %q", bogn.logprefix, logprefix, name)
		return index, nil
	}
	panic(fmt.Errorf("invalid memstore %q", bogn.memstore))
}

func (bogn *Bogn) builddiskstore(
	logprefix string,
	level, version int, sha, flushunix string, settstodisk s.Settings,
	itere api.EntryIterator, appendid string, valuelogs []string,
	what string, appdata []byte) (index api.Index, err error) {

	switch bogn.diskstore {
	case "bubt":
		index, err = bogn.builddiskbubt(
			logprefix, level, version, sha, flushunix, settstodisk, itere,
			appendid, valuelogs, what, appdata,
		)
		fmsg := "%v %v: new bubt snapshot %q"
		infof(fmsg, bogn.logprefix, logprefix, index.ID())
		return
	}
	panic("impossible situation")
}

func (bogn *Bogn) builddiskbubt(
	logprefix string,
	level, version int, sha, flushunix string, settstodisk s.Settings,
	itere api.EntryIterator, appendid string, valuelogs []string,
	what string, appdata []byte) (index api.Index, err error) {

	// book-keep largest seqno for this snapshot.
	var diskseqno, count uint64
	eof := &eofentry{}

	wrap := func(fin bool) (entry api.IndexEntry) {
		if itere != nil {
			entry = itere(fin)
			_, seqno, _, e := entry.Key()
			if seqno > diskseqno {
				diskseqno = seqno
			}
			if e == nil {
				count++
			}
			return
		}
		return eof
	}

	now := time.Now()
	dirname := bogn.levelname(level, version, sha)

	bubtsetts := bogn.setts.Section("bubt.").Trim("bubt.")
	paths := bubtsetts.Strings("diskpaths")
	msize := bubtsetts.Int64("mblocksize")
	zsize := bubtsetts.Int64("zblocksize")
	vsize := bubtsetts.Int64("vblocksize")
	bt, err := bubt.NewBubt(dirname, paths, msize, zsize, vsize)
	if err != nil {
		errorf("%v NewBubt(): %v", bogn.logprefix, err)
		return nil, err
	}

	// futher configure bubt builder.
	if what == "compact.tombstonepurge" {
		bt.TombstonePurge(true)

	} else if bogn.isappendvlogs(vsize, what, valuelogs, paths) {
		bt.AppendValuelogs(vsize, appendid, valuelogs)
	}

	// build
	if err = bt.Build(wrap, nil); err != nil {
		errorf("%v Build(): %v", bogn.logprefix, err)
		return nil, err
	}
	mwmetadata := bogn.mwmetadata(diskseqno, flushunix, appdata, settstodisk)
	if _, err = bt.Writemetadata(mwmetadata); err != nil {
		errorf("%v Writemetadata(): %v", bogn.logprefix, err)
		return nil, err
	}
	bt.Close()

	// TODO: make this as separate function and let it be called
	// with more customization in dopersist, doflush, findisk, dowindup.
	mmap := bubtsetts.Bool("mmap")
	snap := bogn.currsnapshot()
	if mmap == false && snap != nil {
		latestlevel, _ := snap.latestlevel()
		if latestlevel < 0 || level <= latestlevel {
			mmap = true
		}
	}
	ndisk, err := bubt.OpenSnapshot(dirname, paths, mmap)
	if err != nil {
		errorf("%v OpenSnapshot(): %v", bogn.logprefix, err)
		return nil, err
	}

	fp := humanize.Bytes(uint64(ndisk.Footprint()))
	payl := humanize.Bytes(uint64(bogn.indexpayload(ndisk)))
	id, elapsed := ndisk.ID(), time.Since(now)
	fmsg := "%v %v: took %v for bubt %v with %v entries, ~%v/%v & mmap:%v\n"
	infof(fmsg, bogn.logprefix, logprefix, elapsed, id, count, payl, fp, mmap)

	return ndisk, nil
}

// open latest versions for each disk level
func (bogn *Bogn) opendisksnaps(
	setts s.Settings) (disks [16]api.Index, err error) {

	switch bogn.diskstore {
	case "bubt":
		bubtsetts := bogn.setts.Section("bubt.").Trim("bubt.")
		diskpaths := bubtsetts.Strings("diskpaths")
		mmap := bubtsetts.Bool("mmap")
		disks, err = bogn.openbubtsnaps(diskpaths, mmap)

	default:
		panic("impossible situation")
	}

	if err != nil {
		return disks, err
	}

	// log information about active disksnapshots.
	n := 0
	for _, disk := range disks {
		if disk == nil {
			continue
		}
		n++
		infof("%v open-disksnapshot %v", bogn.logprefix, disk.ID())
	}
	if n > 1 {
		bogn.dgmstate = 1
	}
	return disks, nil
}

// open latest versions for each disk level from bubt snapshots.
func (bogn *Bogn) openbubtsnaps(
	paths []string, mmap bool) ([16]api.Index, error) {

	var disks [16]api.Index

	dircache := map[string]bool{}
	for _, path := range paths {
		fis, err := ioutil.ReadDir(path)
		if err != nil {
			errorf("%v openbubtsnaps.ReadDir(): %v", bogn.logprefix, err)
			return disks, err
		}
		for _, fi := range fis {
			if !fi.IsDir() {
				continue
			}
			dirname := fi.Name()
			if _, ok := dircache[dirname]; ok {
				continue
			}
			level, _, _ := bogn.path2level(dirname)
			if level < 0 {
				continue // not a bogn disk level
			}
			disk, err := bubt.OpenSnapshot(dirname, paths, mmap)
			if err != nil {
				return disks, err
			}
			if disks[level] != nil {
				panic("impossible situation")
			}
			disks[level] = disk
			dircache[dirname] = true
		}
	}
	return disks, nil
}

// compact away older versions in disk levels.
func (bogn *Bogn) compactdisksnaps(
	logprefix, diskstore string, diskpaths []string, merge bool) error {

	switch diskstore {
	case "bubt":
		return bogn.compactbubtsnaps(logprefix, diskpaths, merge)
	}
	panic(fmt.Errorf("invalid diskstore %v", diskstore))
}

func (bogn *Bogn) compactbubtsnaps(
	logprefix string, diskpaths []string, merge bool) error {

	var disks [16]api.Index

	mmap, dircache := false, map[string]bool{}
	for _, path := range diskpaths {
		fis, err := ioutil.ReadDir(path)
		if err != nil {
			errorf("%v compactbubtsnaps.ReadDir(): %v", bogn.logprefix, err)
			return err
		}
		for _, fi := range fis {
			if !fi.IsDir() {
				continue
			}
			dirname := fi.Name()
			if _, ok := dircache[dirname]; ok {
				continue
			}
			level, version, _ := bogn.path2level(dirname)
			if level < 0 {
				continue // not a bogn directory
			}
			disk, err := bubt.OpenSnapshot(dirname, diskpaths, mmap)
			if err != nil { // bad snapshot
				bubt.PurgeSnapshot(dirname, diskpaths)
				continue
			}
			if od := disks[level]; od == nil { // first version
				disks[level] = disk

			} else if _, over, _ := bogn.path2level(od.ID()); over < version {
				fmsg := "%v %v: compact away older version %v"
				infof(fmsg, bogn.logprefix, logprefix, od.ID())
				bogn.destroylevels(od)
				disks[level] = disk

			} else {
				fmsg := "%v %v: compact away older version %v"
				infof(fmsg, bogn.logprefix, logprefix, disk.ID())
				bogn.destroylevels(disk)
			}
			dircache[dirname] = true
		}
	}

	validdisks := []api.Index{}
	for _, disk := range disks {
		if disk != nil {
			validdisks = append(validdisks, disk)
		}
	}
	if len(validdisks) == 0 {
		fmsg := "%v %v: no disk levels found for compaction"
		infof(fmsg, bogn.logprefix, logprefix)
	} else if len(validdisks) == 1 {
		fmsg := "%v %v: no compaction, single disk snapshot found"
		infof(fmsg, bogn.logprefix, logprefix)
	} else if merge {
		return bogn.mergedisksnapshots(logprefix, validdisks)
	}
	bogn.closelevels(validdisks...)
	return nil
}

func (bogn *Bogn) mergedisksnapshots(
	logprefix string, disks []api.Index) error {

	scans := make([]api.EntryIterator, 0)
	sourceids := []string{}
	for _, disk := range disks {
		if itere := disk.ScanEntries(); itere != nil {
			scans = append(scans, itere)
		}
		sourceids = append(sourceids, disk.ID())
	}

	// get latest settings, from latest disk, including memversions and
	// diskversions and use them when building the merged snapshot.
	disksetts := bogn.settingsfromdisk(disks[0])
	flushunix := bogn.getflushunix(disks[0])
	appdata := bogn.getappdata(disks[0])
	bogn.setts = disksetts

	fmsg := "%v %v: merging [%v]"
	infof(fmsg, bogn.logprefix, logprefix, strings.Join(sourceids, ","))

	itere := reduceitere(scans)
	level, uuid := 15, bogn.newuuid()
	diskversions := bogn.getdiskversions(disks[0])
	version := diskversions[level] + 1
	ndisk, err := bogn.builddiskstore(
		logprefix, level, version, uuid, flushunix, disksetts, itere,
		"" /*appendid*/, nil /*valuelogs*/, "offlinemerge", appdata,
	)
	if err != nil {
		return err
	}
	itere(true /*fin*/)

	ndisk.Close()

	for _, disk := range disks[:] {
		if disk != nil {
			infof("%v %v: merged out %q", bogn.logprefix, logprefix, disk.ID())
			disk.Close()
			disk.Destroy()
		}
	}
	return nil
}

func (bogn *Bogn) destroydisksnaps(
	logprefix, logpath, diskstore string, diskpaths []string) error {

	// purge log dir
	paths := make([]string, len(diskpaths))
	copy(paths, diskpaths)
	if len(logpath) > 0 {
		paths = append(paths, logpath)
	}
	if err := bogn.destorybognlogs(logprefix, paths); err != nil {
		return err
	}

	// purge disk snapshots
	switch diskstore {
	case "bubt":
		if err := bogn.destroybubtsnaps(logprefix, diskpaths); err != nil {
			return err
		}
		return nil
	}
	panic("unreachable code")
}

func (bogn *Bogn) destorybognlogs(logprefix string, diskpaths []string) error {
	for _, path := range diskpaths {
		logdir := bogn.logdir(path)
		if fi, err := os.Stat(logdir); err != nil {
			continue

		} else if fi.IsDir() {
			if err := os.RemoveAll(logdir); err != nil {
				errorf("%v RemoveAll(%q): %v", bogn.logprefix, logdir, err)
				return err
			}
			infof("%v %v: removed logdir %q", bogn.logprefix, logprefix, logdir)
			return nil
		}
	}
	infof("%v %v: no logdir found !!", bogn.logprefix, logprefix)
	return nil
}

func (bogn *Bogn) destroybubtsnaps(logprefix string, diskpaths []string) error {
	pathlist := strings.Join(diskpaths, ", ")
	for _, path := range diskpaths {
		fis, err := ioutil.ReadDir(path)
		if err != nil {
			errorf("%v destroybubtsnaps.ReadDir(): %v", bogn.logprefix, err)
			return err
		}
		for _, fi := range fis {
			if !fi.IsDir() {
				continue
			}
			level, _, _ := bogn.path2level(fi.Name())
			if level < 0 {
				continue // not a bogn directory
			}
			fmsg := "%v %v: purge bubt snapshot %q under %q"
			infof(fmsg, bogn.logprefix, logprefix, fi.Name(), pathlist)
			bubt.PurgeSnapshot(fi.Name(), diskpaths)
		}
	}
	return nil
}

// release resources held in disk levels.
func (bogn *Bogn) closelevels(indexes ...api.Index) {
	for _, index := range indexes {
		if index != nil {
			index.Close()
		}
	}
}

// destroy disk levels.
func (bogn *Bogn) destroylevels(indexes ...api.Index) {
	for _, index := range indexes {
		if index != nil {
			index.Close()
			index.Destroy()
		}
	}
}

func (bogn *Bogn) currdiskversion(level int) int {
	return bogn.diskversions[level]
}

func (bogn *Bogn) nextdiskversion(level int) int {
	bogn.diskversions[level]++
	return bogn.diskversions[level]
}

func (bogn *Bogn) logstore(index api.Index) {
	switch idx := index.(type) {
	case *llrb.LLRB:
		idx.Log()
	case *llrb.MVCC:
		idx.Log()
	case *bubt.Snapshot:
		idx.Log()
	}
}

func (bogn *Bogn) validatestore(index api.Index) {
	switch idx := index.(type) {
	case *llrb.LLRB:
		idx.Validate()
	case *llrb.MVCC:
		idx.Validate()
	}
}

func (bogn *Bogn) getdiskpaths() []string {
	switch bogn.diskstore {
	case "bubt":
		bubtsetts := bogn.setts.Section("bubt.").Trim("bubt.")
		return bubtsetts.Strings("diskpaths")
	}
	panic("impossible situation")
}

func (bogn *Bogn) getdiskseqno(disk api.Index) uint64 {
	metadata := bogn.diskmetadata(disk)
	return metadata["seqno"].(uint64)
}

func (bogn *Bogn) getflushunix(disk api.Index) string {
	metadata := bogn.diskmetadata(disk)
	return metadata["flushunix"].(string)
}

func (bogn *Bogn) getappdata(disk api.Index) []byte {
	metadata := bogn.diskmetadata(disk)
	appdatastr := metadata["appdata"].(string)
	appdata, _ := base64.StdEncoding.DecodeString(appdatastr)
	return appdata
}

func (bogn *Bogn) getdiskversions(disk api.Index) [16]int {
	metadata := bogn.diskmetadata(disk)
	return metadata["diskversions"].([16]int)
}

// return actual footprint on disk for index.
func (bogn *Bogn) indexfootprint(index api.Index) int64 {
	if index == nil {
		return 0
	}
	switch idx := index.(type) {
	case *llrb.LLRB:
		if idx == nil {
			return 0
		}
		return idx.Footprint()

	case *llrb.MVCC:
		if idx == nil {
			return 0
		}
		return idx.Footprint()

	case *bubt.Snapshot:
		if idx == nil {
			return 0
		}
		return idx.Footprint()
	}
	panic("unreachable code")
}

func (bogn *Bogn) indexseqno(index api.Index) uint64 {
	if index == nil {
		return 0
	}
	switch idx := index.(type) {
	case *llrb.LLRB:
		if idx == nil {
			return 0
		}
		return idx.Getseqno()

	case *llrb.MVCC:
		if idx == nil {
			return 0
		}
		return idx.Getseqno()

	case *bubt.Snapshot:
		return bogn.getdiskseqno(index)
	}
	panic("unreachable code")
}

// return number of entries in the index
func (bogn *Bogn) indexcount(index api.Index) int64 {
	if index == nil {
		return 0
	}
	switch idx := index.(type) {
	case *llrb.LLRB:
		if idx == nil {
			return 0
		}
		return idx.Count()

	case *llrb.MVCC:
		if idx == nil {
			return 0
		}
		return idx.Count()

	case *bubt.Snapshot:
		if idx == nil {
			return 0
		}
		return idx.Count()
	}
	panic("unreachable code")
}

// returns approximate payload in index.
func (bogn *Bogn) indexpayload(index api.Index) int64 {
	if index == nil {
		return 0
	}
	switch idx := index.(type) {
	case *bubt.Snapshot:
		if idx == nil {
			return 0
		}
		info := idx.Info()
		payload := info.Int64("keymem") + info.Int64("valmem")
		payload += (idx.Count() * 24)
		return payload
	}
	panic("unreachable code")
}

// return the oldest snapshots value-logs.
func (bogn *Bogn) indexvaluelogs(disks []api.Index) (string, []string) {
	if len(disks) == 0 {
		return "", nil
	}
	disk := disks[len(disks)-1]
	if disk == nil {
		return "", nil
	}
	switch index := disk.(type) {
	case *bubt.Snapshot:
		if index == nil {
			return "", nil
		}
		return index.ID(), index.Valuelogs()
	}
	panic("unreachable code")
}

func (bogn *Bogn) diskwritebytes(disk api.Index) int64 {
	if disk == nil {
		return 0
	}
	switch index := disk.(type) {
	case *bubt.Snapshot:
		info := index.Info()
		zsize := info.Int64("zblocksize")
		msize := info.Int64("mblocksize")
		vsize := info.Int64("vblocksize")
		wramplification := info.Int64("n_zblocks") * zsize
		wramplification += info.Int64("n_mblocks") * msize
		n_vblocks := info.Int64("n_vblocks")
		n_ablocks := info.Int64("n_ablocks")
		wramplification += (n_vblocks - n_ablocks) * vsize

		numpaths := info.Int64("numpaths")
		wramplification += numpaths * bubt.MarkerBlocksize
		if vsize > 0 {
			wramplification += numpaths * bubt.MarkerBlocksize
		}

		return wramplification
	}
	panic("unreachable code")
}

func (bogn *Bogn) diskmetadata(disk api.Index) map[string]interface{} {
	metadata := make(map[string]interface{})

	switch d := disk.(type) {
	case *bubt.Snapshot:
		err := json.Unmarshal(d.Metadata(), &metadata)
		if err != nil {
			panic(err)
		}

		setts := s.Settings(metadata).Section("bogn.").Trim("bogn.")
		metadata = map[string]interface{}(setts)

		// cure `seqno`
		val := metadata["seqno"].(string)
		seqno, err := strconv.ParseUint(strings.Trim(val, `"`), 10, 64)
		if err != nil {
			panic(err)
		}
		metadata["seqno"] = seqno

		// cure memversions
		mvers := metadata["memversions"].([]interface{})
		metadata["memversions"] = [3]int{
			int(mvers[0].(float64)),
			int(mvers[1].(float64)),
			int(mvers[2].(float64)),
		}

		// cure diskversions
		diskversions := [16]int{}
		for i, v := range metadata["diskversions"].([]interface{}) {
			diskversions[i] = int(v.(float64))
		}
		metadata["diskversions"] = diskversions

		return metadata
	}
	panic("unreachable code")
}

func (bogn *Bogn) addamplification(ndisk api.Index) {
	if ndisk != nil {
		atomic.AddInt64(&bogn.wramplification, bogn.diskwritebytes(ndisk))
	}
}

func (bogn *Bogn) newuuid() string {
	uuid, err := lib.Newuuid(make([]byte, 8))
	if err != nil {
		panic(err)
	}
	uuidb := make([]byte, 16)
	return string(uuidb[:uuid.Format(uuidb)])
}

func (bogn *Bogn) logstatistics(logprefix string) {
	n := humanize.Bytes(uint64(atomic.LoadInt64(&bogn.wramplification)))
	infof("%v %v: write amplifications %v", bogn.logprefix, logprefix, n)
}

func (bogn *Bogn) isappendvlogs(
	vsize int64, what string, vlogs, paths []string) bool {

	if vsize <= 0 {
		return false
	} else if len(vlogs) == 0 {
		return false
	} else if len(vlogs) != len(paths) {
		panic("impossible situation")
	}

	ok := what == "flush.aggressive" || what == "flush.merge"
	ok = ok || what == "compact.aggregation" || what == "compact.ratio"
	ok = ok || what == "compact.period"
	return ok
}
