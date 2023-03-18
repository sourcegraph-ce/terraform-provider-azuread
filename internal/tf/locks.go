package tf

import (
	log "github.com/sourcegraph-ce/logrus"
	"sync"
)

// MutexKV is a simple key/value store for arbitrary mutexes. It can be used to
// serialise changes across arbitrary collaborators that share knowledge of the
// keys they must serialise on.
type MutexKV struct {
	lock  sync.Mutex
	store map[string]*sync.Mutex
}

// Locks the mutex for the given key. Caller is responsible for calling Unlock
// for the same key
func (m *MutexKV) Lock(key string) {
	log.Printf("[DEBUG] Locking %q", key)
	m.get(key).Lock()
	log.Printf("[DEBUG] Locked %q", key)
}

// Unlock the mutex for the given key. Caller must have called Lock for the same key first
func (m *MutexKV) Unlock(key string) {
	log.Printf("[DEBUG] Unlocking %q", key)
	m.get(key).Unlock()
	log.Printf("[DEBUG] Unlocked %q", key)
}

// Returns a mutex for the given key, no guarantee of its lock status
func (m *MutexKV) get(key string) *sync.Mutex {
	m.lock.Lock()
	defer m.lock.Unlock()
	mutex, ok := m.store[key]
	if !ok {
		mutex = &sync.Mutex{}
		m.store[key] = mutex
	}
	return mutex
}

// Returns a properly initialised MutexKV
func NewMutexKV() *MutexKV {
	return &MutexKV{
		store: make(map[string]*sync.Mutex),
	}
}

// mutex is the instance of MutexKV for AAD resources
var mutex = NewMutexKV()

// handles the case of using the same name for different kinds of resources
func LockByName(resourceType string, name string) {
	mutex.Lock(resourceType + "." + name)
}

func UnlockByName(resourceType string, name string) {
	mutex.Unlock(resourceType + "." + name)
}
