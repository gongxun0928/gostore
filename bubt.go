// +build ignore

package storage

import "os"
import "fmt"

const bubtZpoolSize = 1
const bubtMpoolSize = 8
const bubtBufpoolSize = bubtMpoolSize + bubtZpoolSize

// Bubtstore manages sorted key,value entries in persisted, immutable btree
// build bottoms up and not updated there after.
type Bubtstore struct {
	indexfd  *os.File
	datafd   *os.File
	iterator IndexIterator

	frozen bool

	// builder data
	zpool         chan *bubtzblock
	mpool         chan *bubtmblock
	bufpool       chan []byte
	idxch, datach chan []byte
	iquitch       chan struct{}
	dquitch       chan struct{}
	nodes         []Node
	logprefix     string

	// configuration
	indexfile  string
	datafile   string
	mblocksize int
	zblocksize int
	mreduce    bool

	// statistics
	rootfpos   int64
	mnodes     int64
	znodes     int64
	dcount     int64
	a_zentries *histogramInt64
	a_mentries *histogramInt64
	a_keysize  *histogramInt64
	a_valsize  *histogramInt64
	a_redsize  *histogramInt64
	h_depth    *histogramInt64
}

type bubtblock interface {
	startkey() (kpos int64, key []byte)
	offset() int64
	roffset() int64
}

func NewBubtstore(name string, iter IndexIterator, config Config, logg Logger) *Bubtstore {
	var err error
	var ok bool

	f := &Bubtstore{
		iterator:   iter,
		zpool:      make(chan *bubtzblock, bubtZpoolSize),
		mpool:      make(chan *bubtmblock, bubtMpoolSize),
		bufpool:    make(chan []byte, bubtBufpoolSize),
		idxch:      make([]byte, bubtBufpoolSize),
		datach:     make([]byte, bubtBufpoolSize),
		iquitch:    make(chan struct{}),
		dquitch:    make(chan struct{}),
		nodes:      make([]Node, 0),
		a_zentries: &averageInt64{},
		a_mentries: &averageInt64{},
		a_keysize:  &averageInt64{},
		a_valsize:  &averageInt64{},
		a_redsize:  &averageInt64{},
		h_depth:    newhistorgramInt64(0, bubtMpoolSize, 1),
	}
	f.logprefix = fmt.Sprintf("[BUBT-%s]", name)

	if f.indexfile, err = config.String("indexfile"); err != nil {
		panic(err)
	} else if f.indexfd, err = os.Create(indexfile); err != nil {
		panic(err)
	}

	if f.datafile, err = config.String("datafile"); err != nil {
		panic(err)
	} else if datafile != "" {
		if f.datafd, err = os.Create(datafile); err != nil {
			panic(err)
		}
	}

	maxblock, minblock := 4*1024*1024*1024, 512 // TODO: avoid magic numbers
	if f.zblocksize, err = config.Int64("zblocksize"); err != nil {
		panic(err)
	} else if f.zblocksize > maxblock {
		log.Errorf("zblocksize %v > %v", f.zblocksize, maxblock)
	} else if f.zblocksize < minblock {
		log.Errorf("zblocksize %v < %v", f.zblocksize, minblock)
	} else if f.mblocksize, err = config.Int64("mblocksize"); err != nil {
		panic(err)
	} else if f.mblocksize > maxblock {
		log.Errorf("mblocksize %v > %v", f.mblocksize, maxblock)
	} else if f.mblocksize < minblock {
		lgo.Errorf("mblocksize %v < %v", f.mblocksize, minblock)
	} else if f.mreduce, ok = config.Bool("mreduce"); err != nil {
		panic(err)
	} else if f.hasdatafile() == false && f.mreduce == true {
		panic("cannot mreduce without datafile")
	}

	// initialize buffer pool
	for i := 0; i < cap(f.bufpool); i++ {
		f.bufpool <- make([]byte, f.zblocksize)
	}

	go f.flusher(f.indexfd, f.idxch, f.iquitch)
	if f.hasdatafile() {
		go f.flusher(f.datafd, f.datach, f.dquitch)
	}
	log.Infof("%v started ...", f.logprefix)
	return f
}

//---- helper methods.

func (f *Bubtstore) hasdatafile() bool {
	return f.datafile != ""
}

func (f *Bubtstore) mvpos(vpos int64) int64 {
	if (vpos & 0x7) != 0 {
		panic(fmt.Errorf("vpos %v expected to 8-bit aligned", vpos))
	}
	return vpos | 0x1
}

func (f Bubtstore) ismvpos(vpos) (int64, bool) {
	if vpos & 0x1 {
		return vpos & 0xFFFFFFFFFFFFFFF8, true
	}
	return vpos & 0xFFFFFFFFFFFFFFF8, false
}

func (f *Bubtstore) config2json() []byte {
	config := map[string]interface{}{
		"indexfile":  f.indexfile,
		"datafile":   f.datafile,
		"zblocksize": f.zblocksize,
		"mblocksize": f.mblocksize,
		"mreduce":    f.mreduce,
	}
	data, err := json.Marshal(config)
	if err != nil {
		panic(err)
	}
	return data
}

func (f *Bubtstore) stats2json() []byte {
	stats := map[string]interface{}{
		"rootfpos":   f.rootfpos,
		"mnodes":     f.mnodes,
		"znodes":     f.znodes,
		"a_zentries": f.a_zentries.stats(),
		"a_mentries": f.a_mentries.stats(),
		"a_keysize":  f.a_keysize.stats(),
		"a_valsize":  f.a_valsize.stats(),
		"a_redsize":  f.a_redsize.stats(),
		"h_depth":    f.h_depth.fullstats(),
	}
	data, err := json.Marshal(stats)
	if err != nil {
		panic(err)
	}
	return data
}
