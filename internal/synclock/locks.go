package synclock

import "sync"

var (
	vaultLocks sync.Map
	pathLocks  sync.Map
)

func Vault(vaultID string) *sync.Mutex {
	value, _ := vaultLocks.LoadOrStore(vaultID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func Path(key string) *sync.Mutex {
	value, _ := pathLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}
