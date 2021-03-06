package lsm

import "io"
import "os"
import "fmt"
import "bytes"
import "testing"
import "math/rand"
import "path/filepath"

import "github.com/bnclabs/gostore/llrb"
import "github.com/bnclabs/gostore/bubt"
import s "github.com/bnclabs/gosettings"

func TestYGetM(t *testing.T) {
	//SetYGetpool(10)
	setts := s.Settings{
		"keycapacity": 1024 * 1024 * 1024,
		"valcapacity": 1024 * 1024 * 1024,
	}
	ref := llrb.NewLLRB("refllrb", setts)

	llrb1, keys := makeLLRB("llrb1", 100000, nil, ref, -1, -1)
	llrb2, keys := makeLLRB("llrb2", 0, keys, ref, 4, 8)
	llrb3, keys := makeLLRB("llrb3", 0, keys, ref, 4, 8)
	llrb4, _ := makeLLRB("llrbr", 0, keys, ref, 4, 8)
	defer llrb1.Destroy()
	defer llrb2.Destroy()
	defer llrb3.Destroy()
	defer llrb4.Destroy()

	t.Logf("llrb1 has %v items", llrb1.Count())
	t.Logf("llrb2 has %v items", llrb2.Count())
	t.Logf("llrb3 has %v items", llrb3.Count())
	t.Logf("llrb4 has %v items", llrb4.Count())

	paths := makepaths()

	name, msize, mmap := "bubt1", int64(4096), false
	t.Logf("bubt1: paths : %v", paths)
	t.Logf("bubt1: msize: %v, zsize: %v, vsize: %v", msize, msize, 0)
	bb, err := bubt.NewBubt(name, paths, msize, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	itere := llrb1.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for llrb1"))
	if err != nil {
		t.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt1, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		t.Fatal(err)
	}

	name, msize, mmap = "bubt2", int64(4096*2), true
	t.Logf("bubt2:: msize: %v, zsize: %v, vsize: %v", msize, msize, 0)
	bb, err = bubt.NewBubt(name, paths, msize, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	itere = llrb2.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for bubt4"))
	if err != nil {
		t.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt2, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		t.Fatal(err)
	}

	defer bubt2.Destroy()
	defer bubt1.Destroy()
	defer bubt2.Close()
	defer bubt1.Close()

	refiter, v := ref.Scan(), make([]byte, 16)
	for key, value, seqno, deleted, err := refiter(false); err != io.EOF; {
		getter := YGet(bubt1.Get, YGet(bubt2.Get, YGet(llrb3.Get, llrb4.Get)))
		v, cas, d, ok := getter(key, v)
		if ok == false {
			t.Errorf("expected key")
		} else if d != deleted {
			t.Errorf("expected %v, got %v", deleted, d)
		} else if cas != seqno {
			t.Errorf("expected %v, got %v", seqno, cas)
		} else if deleted == false && bytes.Compare(value, v) != 0 {
			t.Errorf("expected %q, got %q", value, v)
		}
		key, value, seqno, deleted, err = refiter(false)
	}
	refiter(true /*fin*/)
}

func TestYGetZ(t *testing.T) {
	//SetYGetpool(10)
	setts := s.Settings{
		"keycapacity": 1024 * 1024 * 1024,
		"valcapacity": 1024 * 1024 * 1024,
	}
	ref := llrb.NewLLRB("refllrb", setts)

	llrb1, keys := makeLLRB("llrb1", 100000, nil, ref, -1, -1)
	llrb2, keys := makeLLRB("llrb2", 0, keys, ref, 4, 8)
	llrb3, keys := makeLLRB("llrb3", 0, keys, ref, 4, 8)
	llrb4, _ := makeLLRB("llrbr", 0, keys, ref, 4, 8)
	defer llrb1.Destroy()
	defer llrb2.Destroy()
	defer llrb3.Destroy()
	defer llrb4.Destroy()

	t.Logf("llrb1 has %v items", llrb1.Count())
	t.Logf("llrb2 has %v items", llrb2.Count())
	t.Logf("llrb3 has %v items", llrb3.Count())
	t.Logf("llrb4 has %v items", llrb4.Count())

	paths := makepaths()

	name, msize, zsize, mmap := "bubt1", int64(4096), int64(4096), false
	t.Logf("bubt1:: msize: %v, zsize: %v, vsize: %v", msize, msize, 0)
	bb, err := bubt.NewBubt(name, paths, msize, zsize, 0)
	if err != nil {
		t.Fatal(err)
	}
	itere := llrb1.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for llrb1"))
	if err != nil {
		t.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt1, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		t.Fatal(err)
	}

	name, msize, zsize, mmap = "bubt2", int64(4096), int64(4096)*2, true
	t.Logf("bubt2:: msize: %v, zsize: %v, vsize: %v", msize, zsize, 0)
	bb, err = bubt.NewBubt(name, paths, msize, zsize, 0)
	if err != nil {
		t.Fatal(err)
	}
	itere = llrb2.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for bubt4"))
	if err != nil {
		t.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt2, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		t.Fatal(err)
	}

	defer bubt2.Destroy()
	defer bubt1.Destroy()
	defer bubt2.Close()
	defer bubt1.Close()

	refiter, v := ref.Scan(), make([]byte, 16)
	for key, value, seqno, deleted, err := refiter(false); err != io.EOF; {
		getter := YGet(bubt1.Get, YGet(bubt2.Get, YGet(llrb3.Get, llrb4.Get)))
		v, cas, d, ok := getter(key, v)
		if ok == false {
			t.Errorf("expected key")
		} else if d != deleted {
			t.Errorf("expected %v, got %v", deleted, d)
		} else if cas != seqno {
			t.Errorf("expected %v, got %v", seqno, cas)
		} else if deleted == false && bytes.Compare(value, v) != 0 {
			t.Errorf("expected %q, got %q", value, v)
		}
		key, value, seqno, deleted, err = refiter(false)
	}
	refiter(true /*fin*/)
}

func TestYGetV(t *testing.T) {
	//SetYGetpool(10)
	setts := s.Settings{
		"keycapacity": 1024 * 1024 * 1024,
		"valcapacity": 1024 * 1024 * 1024,
	}
	ref := llrb.NewLLRB("refllrb", setts)

	llrb1, keys := makeLLRB("llrb1", 100000, nil, ref, -1, -1)
	llrb2, keys := makeLLRB("llrb2", 0, keys, ref, 4, 8)
	llrb3, keys := makeLLRB("llrb3", 0, keys, ref, 4, 8)
	llrb4, _ := makeLLRB("llrbr", 0, keys, ref, 4, 8)
	defer llrb1.Destroy()
	defer llrb2.Destroy()
	defer llrb3.Destroy()
	defer llrb4.Destroy()

	t.Logf("llrb1 has %v items", llrb1.Count())
	t.Logf("llrb2 has %v items", llrb2.Count())
	t.Logf("llrb3 has %v items", llrb3.Count())
	t.Logf("llrb4 has %v items", llrb4.Count())

	paths := makepaths()
	t.Logf("paths :%v", paths)

	name, msize, vsize, mmap := "bubt1", int64(4096), int64(4096*2), false
	t.Logf("bubt2:: msize: %v, zsize: %v, vsize: %v", msize, msize, vsize)
	bb, err := bubt.NewBubt(name, paths, msize, msize, vsize)
	if err != nil {
		t.Fatal(err)
	}
	itere := llrb1.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for llrb1"))
	if err != nil {
		t.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt1, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		t.Fatal(err)
	}

	name, zsize, vsize, mmap := "bubt2", int64(4096*2), int64(4096)*4, true
	t.Logf("bubt2:: msize: %v, zsize: %v, vsize: %v", msize, zsize, vsize)
	bb, err = bubt.NewBubt(name, paths, msize, zsize, vsize)
	if err != nil {
		t.Fatal(err)
	}
	itere = llrb2.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for bubt4"))
	if err != nil {
		t.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt2, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		t.Fatal(err)
	}

	//defer bubt2.Destroy()
	//defer bubt1.Destroy()
	defer bubt2.Close()
	defer bubt1.Close()

	refiter, v := ref.Scan(), make([]byte, 16)
	for key, value, seqno, deleted, err := refiter(false); err != io.EOF; {
		getter := YGet(bubt1.Get, YGet(bubt2.Get, YGet(llrb3.Get, llrb4.Get)))
		v, cas, d, ok := getter(key, v)
		if ok == false {
			t.Errorf("expected key")
		} else if d != deleted {
			t.Errorf("expected %v, got %v", deleted, d)
		} else if cas != seqno {
			t.Errorf("expected %v, got %v", seqno, cas)
		} else if deleted == false && bytes.Compare(value, v) != 0 {
			t.Errorf("expected %q, got %q", value, v)
		}
		key, value, seqno, deleted, err = refiter(false)
	}
	refiter(true /*fin*/)
}

func BenchmarkYGetM(b *testing.B) {
	setts := s.Settings{
		"keycapacity": 1024 * 1024 * 1024,
		"valcapacity": 1024 * 1024 * 1024,
	}
	ref := llrb.NewLLRB("refllrb", setts)

	llrb1, nkeys := makeLLRB("llrb1", b.N, nil, ref, -1, -1)
	llrb2, keys := makeLLRB("llrb2", 0, nkeys, ref, 4, 8)
	llrb3, keys := makeLLRB("llrb3", 0, keys, ref, 4, 8)
	llrb4, _ := makeLLRB("llrbr", 0, keys, ref, 4, 8)
	defer llrb1.Destroy()
	defer llrb2.Destroy()
	defer llrb3.Destroy()
	defer llrb4.Destroy()

	paths := makepaths()

	name, msize, zsize, mmap := "bubt1", int64(4096), int64(4096), false
	bb, err := bubt.NewBubt(name, paths, msize, zsize, 0)
	if err != nil {
		b.Fatal(err)
	}
	itere := llrb1.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for llrb1"))
	if err != nil {
		b.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt1, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		b.Fatal(err)
	}

	name, msize, zsize, mmap = "bubt2", int64(4096), int64(4096)*2, true
	bb, err = bubt.NewBubt(name, paths, msize, zsize, 0)
	if err != nil {
		b.Fatal(err)
	}
	itere = llrb2.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for bubt4"))
	if err != nil {
		b.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt2, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		b.Fatal(err)
	}

	defer bubt2.Destroy()
	defer bubt1.Destroy()
	defer bubt2.Close()
	defer bubt1.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		getter := YGet(bubt1.Get, YGet(bubt2.Get, YGet(llrb3.Get, llrb4.Get)))
		getter(nkeys[i], nil)
	}
}

func BenchmarkYGetV(b *testing.B) {
	setts := s.Settings{
		"keycapacity": 1024 * 1024 * 1024,
		"valcapacity": 1024 * 1024 * 1024,
	}
	ref := llrb.NewLLRB("refllrb", setts)

	llrb1, nkeys := makeLLRB("llrb1", b.N, nil, ref, -1, -1)
	llrb2, keys := makeLLRB("llrb2", 0, nkeys, ref, 4, 8)
	llrb3, keys := makeLLRB("llrb3", 0, keys, ref, 4, 8)
	llrb4, _ := makeLLRB("llrbr", 0, keys, ref, 4, 8)
	defer llrb1.Destroy()
	defer llrb2.Destroy()
	defer llrb3.Destroy()
	defer llrb4.Destroy()

	paths := makepaths()

	name, msize, vsize, mmap := "bubt1", int64(4096), int64(4096), false
	bb, err := bubt.NewBubt(name, paths, msize, msize, vsize)
	if err != nil {
		b.Fatal(err)
	}
	itere := llrb1.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for llrb1"))
	if err != nil {
		b.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt1, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		b.Fatal(err)
	}

	name, msize, vsize, mmap = "bubt2", int64(4096), int64(4096)*2, true
	bb, err = bubt.NewBubt(name, paths, msize, msize, vsize)
	if err != nil {
		b.Fatal(err)
	}
	itere = llrb2.ScanEntries()
	err = bb.Build(itere, []byte("this is metadata for bubt4"))
	if err != nil {
		b.Fatal(err)
	}
	bb.Close()
	itere(true /*fin*/)

	bubt2, err := bubt.OpenSnapshot(name, paths, mmap)
	if err != nil {
		b.Fatal(err)
	}

	defer bubt2.Destroy()
	defer bubt1.Destroy()
	defer bubt2.Close()
	defer bubt1.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		getter := YGet(bubt1.Get, YGet(bubt2.Get, YGet(llrb3.Get, llrb4.Get)))
		getter(nkeys[i], nil)
	}
}

func makeLLRB(
	name string, n int, keys [][]byte,
	ref *llrb.LLRB, mod1, mod2 int) (*llrb.LLRB, [][]byte) {

	setts := s.Settings{"memcapacity": 1024 * 1024 * 1024}
	mi := llrb.NewLLRB(name, setts)
	mi.Setseqno(ref.Getseqno())

	if len(keys) > 0 {
		newkeys := [][]byte{}
		for i, key := range keys {
			x := fmt.Sprintf("%d", i)
			val := append([]byte(name), x...)
			if i%mod2 == 0 {
				mi.Delete(key, nil, true /*lsm*/)
				ref.Delete(key, nil, true /*lsm*/)
				newkeys = append(newkeys, key)
			} else if i%mod1 == 0 {
				mi.Set(key, val, nil)
				ref.Set(key, val, nil)
				newkeys = append(newkeys, key)
			}
		}
		return mi, newkeys
	}

	keys = [][]byte{}
	for i := 0; i < n; i++ {
		k, v := []byte("key000000000000"), []byte("val00000000000000")
		x := fmt.Sprintf("%d", i)
		key, val := append(k[:3], x...), append(v[:3], x...)
		mi.Set(key, val, nil)
		ref.Set(key, val, nil)
		if i%10 == 0 {
			mi.Delete(key, nil, true /*lsm*/)
			ref.Delete(key, nil, true /*lsm*/)
		}
		keys = append(keys, key)
	}
	return mi, keys
}

func makepaths() []string {
	path, paths := os.TempDir(), []string{}
	dirs := []string{"1", "2", "3"}
	n := 1 + (rand.Intn(len(dirs)) % len(dirs))
	for _, base := range dirs[:n] {
		paths = append(paths, filepath.Join(path, base))
	}
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			panic(err)
		}
	}
	return paths
}
