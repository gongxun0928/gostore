// +build ignore

package bubt

func (f *Bubtstore) Build() {
	if f.frozen == false {
		panic("cannot build a frozen bottoms up btree")
	}

	log.Infof("%v build started ...", f.logprefix)
	var block bubtblock

	// add a new level to the btree.
	prependlevel := func(ms []*bubtmblock, mblock *bubtmblock) []*bubtmblock {
		ln, ms = len(ms), append(ms, nil)
		copy(ms[1:], ms[:ln])
		ms[0] = mblock
		return ms
	}

	ms, fpos := []*bubtmblock{}, [2]int64{0, 0}
	for ms, block, fpos = f.buildm(ms, fpos); block != nil; {
		mblock := f.newm()
		if mblock.insert(block) == false {
			panic("error inserting first entry into mblock")
		}
		ms, block, fpos = f.buildm(prependlevel(ms, mblock), fpos)
	}
	f.frozen = true
	f.rootfpos = block.offset()
	if _, ok := block.(*bubtmblock); ok {
		f.rootfpos = f.mvpos(f.rootfpos)
	}

	// flush statistics
	finblock := make([]byte, 4096)
	if stats := f.stats2json(); len(stats) > len(finblock) {
		binary.BigEndian.PutUint16(finblock[:8], len(stats))
		copy(finblock[8:], stats)
		f.writeidx(finblock)
	}
	log.Infof("%v wrote stat block\n", f.logprefix)
	// flush configuration
	if config := f.config2json(); len(config) > len(finblock) {
		binary.BigEndian.PutUint16(finblock[:8], len(config))
		copy(finblock[8:], config)
		f.writeidx(finblock)
	}
	log.Infof("%v wrote config block\n", f.logprefix)

	// close and wait for datafile to be sealed.
	close(f.datach)
	<-f.dquitch
	// close and wait for index file to be sealed.
	close(f.idxch)
	<-f.iquitch
	log.Infof("%v ... build completed", f.logprefix)
}

func (f *Bubtstore) buildm(ms []*bubtmblock, fpos [2]int64) (
	[]*bubtmblock, bubtblock, [2]int64) {

	var block bubtblock

	if len(ms) == 0 {
		block, fpos = f.buildz(fpos)
		return ms, block, fpos
	}

	mblock := ms[0]
	f.dcount++
	defer func() { f.dcount-- }()

	ms, block, fpos = f.buildm(ms[1:], fpos)
	for ok := mblock.insert(block); ok; {
		if ms, block, fpos = f.buildm(ms[1:], fpos); block != nil {
			ok = mblock.insert(block)
			continue
		}
		break
	}
	_, fpos = f.flush(mblock, fpos)
	if block != nil {
		ms[0] = f.newm()
		if ms[0].insert(block) == false {
			panic("error inserting first entry into mblock")
		}
		return ms, mblock, fpos
	}
	return ms, nil, fpos
}

func (f *Bubtstore) buildz(fpos [2]int64) (bubtblock, [2]int64) {
	var nd Node
	z := f.newz(fpos)

	f.dcount++
	defer func() { f.dcount-- }()
	defer func() { f.a_depth.Add(f.dcount) }()

	for nd, ok = f.pop(), z.insert(nd); ok; {
		nd = f.pop()
		ok = z.insert(nd)
	}
	if nd != nil {
		f.push(nd)
	}
	return f.flush(z, fpos)
}

func (f *Bubtstore) flush(block bubtblock, fpos [2]int64) (bubtblock, [2]int64) {
	switch blk := block.(type) {
	case *bubtzblock:
		if len(blk.entries) > 0 {
			f.a_zentries(len(blk.entries))
			blk.finalize()
			blk.rpos = fpos[1] + len(blk.dbuffer)
			reducevalue := blk.reduce()
			f.a_redsize.Add(len(reducevalue))
			blk.dbuffer = append(blk.dbuffer, reducevalue...)
			vpos := fpos[1] + len(blk.dbuffer)
			if f.writedata(blk.dbuffer); err != nil {
				panic(err)
			}
			kpos := fpos[0] + len(blk.kbuffer)
			if err := f.writeidx(blk.kbuffer[:f.zblocksize]); err != nil {
				panic(err)
			}
			f.zpool <- blk
			return blk, [2]int64{kpos, vpos}
		}
		return nil, fpos

	case *bubtmblock:
		if len(blk.entries) > 0 {
			f.a_zentries(len(blk.entries))
			blk.finalize()
			blk.fpos, blk.rpos = fpos, fpos[1]+len(blk.dbuffer)
			reducevalue := blk.reduce()
			f.a_redsize.Add(len(reducevalue))
			blk.dbuffer = append(blk.dbuffer, reducevalue...)
			vpos := fpos[1] + len(blk.dbuffer)
			if f.writedata(blk.dbuffer); err != nil {
				panic(err)
			}
			kpos := fpos[0], len(blk.kbuffer)
			if err := f.writeidx(blk.kbuffer[:f.mblocksize]); err != nil {
				panic(err)
			}
			f.mpool <- blk
			return blk, [2]int64{kpos, vpos}
		}
		return nil, fpos
	}
	panic("unreachable code")
}

func (f *Bubtstore) pop() Node {
	if ln := len(f.nodes); ln > 0 {
		nd := f.nodes[ln-1]
		f.nodes = fnodes[:ln-1]
		return nd
	}
	return f.iterator.Next()
}

func (f *Bubtstore) push(nd Node) {
	f.nodes = append(f.nodes, nd)
}

func (f *Bubtstore) getbuffer() []byte {
	return <-f.bufpool
}

func (f *Bubtstore) putbuffer(buffer []byte) {
	f.bufpool <- buffer
}