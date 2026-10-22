// Package state persists infrastructure identity. Kubernetes state is still probed on guests.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type Inventory struct {
	Version     int    `json:"version"`
	ClusterID   string `json:"clusterID"`
	ClusterName string `json:"clusterName"`
	Remote      string `json:"remote"`
	Endpoint    string `json:"endpoint"`
	Project     string `json:"project"`
	Machine     string `json:"machine"`
	ResourceID  string `json:"resourceID,omitempty"`
	ConfigHash  string `json:"configHash"`
	Provisioned bool   `json:"provisioned"`
	K3sReady    bool   `json:"k3sReady"`
	Address     string `json:"address,omitempty"`
	Image       string `json:"image,omitempty"`
}

type Store struct {
	path string
	lock *os.File
}

func Path(specPath string) string {
	return filepath.Join(filepath.Dir(specPath), ".quick8s", filepath.Base(specPath)+".state.json")
}

// Open holds an advisory OS lock for the entire lifecycle command. A crashed
// process releases it automatically; the lock file itself is never removed.
func Open(specPath string) (*Store, error) {
	p := Path(specPath)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another quick8s command holds %s: %w", p, err)
	}
	return &Store{path: p, lock: f}, nil
}

func (s *Store) Close()                    { s.lock.Close() }
func (s *Store) Load() (*Inventory, error) { return Read(s.path) }

func Read(path string) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var inv Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("reading inventory %s: %w", path, err)
	}
	if inv.Version != 1 || inv.ClusterID == "" || inv.Machine == "" || inv.ConfigHash == "" || inv.Endpoint == "" {
		return nil, fmt.Errorf("invalid or unsupported inventory %s; restore it before proceeding", path)
	}
	return &inv, nil
}

func (s *Store) Save(inv *Inventory) error {
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".inventory-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}

func (s *Store) Remove() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
