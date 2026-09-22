package httpserver

import "net/http"

// Member paths and admin paths can name the same file through different roots.
// Always compare locks in the physical browser namespace supplied by storage.
func (d *RESTDispatcher) lockPath(path string) (string, error) {
	if mapper, ok := d.read.(davLockPathMapper); ok {
		mapped, err := mapper.LockPath(path)
		if err != nil {
			return "", err
		}
		return canonicalRemotePath(mapped)
	}
	return canonicalRemotePath(path)
}

func (d *RESTDispatcher) checkLocks(w http.ResponseWriter, r *http.Request, paths ...string) (bool, error) {
	mapped := make([]string, 0, len(paths))
	for _, path := range paths {
		canonical, err := d.lockPath(path)
		if err != nil {
			return false, err
		}
		mapped = append(mapped, canonical)
	}
	return checkLocks(w, r, d.locks, true, mapped...)
}

func (d *RESTDispatcher) allowsTree(path string, tokens map[string]struct{}) (bool, error) {
	mapped, err := d.lockPath(path)
	if err != nil {
		return false, err
	}
	return d.locks.allowsTree(mapped, tokens), nil
}

func (d *RESTDispatcher) allowsLock(path string, tokens map[string]struct{}) (bool, error) {
	mapped, err := d.lockPath(path)
	if err != nil {
		return false, err
	}
	return d.locks.Allows(mapped, tokens), nil
}
