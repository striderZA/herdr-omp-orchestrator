package state

import "fmt"

// ProcessIdentity distinguishes a process from a later process that reuses its
// PID. On Windows StartTime is the creation time. On Linux it is the start time
// in clock ticks since boot (field 22 of /proc/<pid>/stat).
type ProcessIdentity struct {
	PID       uint32
	StartTime int64
}

func (identity ProcessIdentity) valid() bool {
	return identity.PID != 0 && identity.StartTime > 0
}

func (identity ProcessIdentity) String() string {
	return fmt.Sprintf("%d/%d", identity.PID, identity.StartTime)
}

func (s *Store) currentProcessIdentity() (ProcessIdentity, error) {
	if !s.processIdentity.valid() {
		return ProcessIdentity{}, fmt.Errorf("%w: current process identity is unavailable", ErrInvalid)
	}
	return s.processIdentity, nil
}

type completedTaskOwner struct {
	identity ProcessIdentity
	taskID   string
}

// MarkTaskComplete is a volatile route-completion fence for one task claim
// owned by a still-live process. It does not change persisted claims or task facts.
func (s *Store) MarkTaskComplete(identity ProcessIdentity, taskID string) {
	if !identity.valid() || taskID == "" {
		return
	}
	s.ownerMu.Lock()
	if s.completedTaskOwners == nil {
		s.completedTaskOwners = make(map[completedTaskOwner]struct{})
	}
	s.completedTaskOwners[completedTaskOwner{identity: identity, taskID: taskID}] = struct{}{}
	s.ownerMu.Unlock()
}

// markTaskActive retires a completed-route fence when this process acquires a
// later claim for the same task.
func (s *Store) markTaskActive(identity ProcessIdentity, taskID string) {
	if !identity.valid() || taskID == "" {
		return
	}
	s.ownerMu.Lock()
	delete(s.completedTaskOwners, completedTaskOwner{identity: identity, taskID: taskID})
	s.ownerMu.Unlock()
}

func (s *Store) taskOwnerAlive(pid, start int64, taskID string) bool {
	if pid == 0 && start == 0 {
		return false // legacy claims rely on their lease
	}
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) || start <= 0 {
		return true // malformed ownership is unknown; never take it over
	}
	identity := ProcessIdentity{PID: uint32(pid), StartTime: start}
	s.ownerMu.RLock()
	_, complete := s.completedTaskOwners[completedTaskOwner{identity: identity, taskID: taskID}]
	s.ownerMu.RUnlock()
	if complete {
		return false
	}
	alive, err := ProcessAlive(identity)
	return err != nil || alive
}
