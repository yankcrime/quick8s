package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryLockAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(path); err == nil {
		other.Close()
		t.Fatal("concurrent lifecycle acquired lock")
	}
	inv := &Inventory{Version: 1, ClusterID: "owner", Machine: "vm", ConfigHash: "hash", Endpoint: "https://example"}
	if err := s.Save(inv); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || *got != *inv {
		t.Fatalf("load=%+v err=%v", got, err)
	}
	info, _ := os.Stat(Path(path))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", info.Mode())
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(); err != nil || got != nil {
		t.Fatalf("after removal=%v err=%v", got, err)
	}
}
