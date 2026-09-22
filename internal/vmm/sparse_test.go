package vmm

import (
	"bytes"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func allocated(t *testing.T, path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

func TestSparseCopyKeepsContentsAndSkipsZeroPages(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "mem"), filepath.Join(dir, "sparse")
	// 3 chunks and a bit: data pages scattered through runs of zeros, including
	// runs that straddle chunk boundaries, and a zero tail.
	size := 3*sparseChunk + 64*sparsePage
	want := make([]byte, size)
	rng := rand.New(rand.NewSource(1))
	dataPages := 0
	for p := 0; p < size/sparsePage-2; p++ {
		if rng.Intn(10) == 0 || p == sparseChunk/sparsePage-1 || p == sparseChunk/sparsePage {
			rng.Read(want[p*sparsePage : (p+1)*sparsePage])
			dataPages++
		}
	}
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	written, err := sparseCopy(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(dataPages)*sparsePage {
		t.Errorf("wrote %d bytes, want %d", written, dataPages*sparsePage)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Fatal("contents differ")
	}
	if a := allocated(t, dst); a > int64(dataPages+16)*sparsePage {
		t.Errorf("copy holds %d bytes for %d data pages", a, dataPages)
	}
}

func TestSparseCopyOddSizeAndDenseFiles(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "mem"), filepath.Join(dir, "sparse")
	b := make([]byte, 5*sparsePage+100)
	b[len(b)-1] = 1
	os.WriteFile(src, b, 0o644)
	if n, err := sparseCopy(src, dst); err != nil || n != 100 {
		t.Fatalf("wrote %d, err %v", n, err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, b) {
		t.Fatal("contents differ")
	}

	// Mostly data: not worth a second file.
	dense := make([]byte, 2*sparseChunk)
	rand.New(rand.NewSource(2)).Read(dense)
	os.WriteFile(src, dense, 0o644)
	if _, err := sparseCopy(src, dst); !errors.Is(err, errDense) {
		t.Fatalf("dense file: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("a dense copy was left behind")
	}
}
