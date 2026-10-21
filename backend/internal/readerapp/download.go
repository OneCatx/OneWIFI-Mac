package readerapp

import (
	"errors"
	"sync"
	"time"
)

type DownloadState string

const (
	Preparing      DownloadState = "PREPARING"
	Authenticating DownloadState = "AUTHENTICATING"
	Downloading    DownloadState = "DOWNLOADING"
	Installing     DownloadState = "INSTALLING"
	Activating     DownloadState = "ACTIVATING"
	Registering    DownloadState = "REGISTERING"
	Completed      DownloadState = "COMPLETED"
	Failed         DownloadState = "FAILED"
)

var downloadOrder = map[DownloadState]int{
	Preparing: 0, Authenticating: 1, Downloading: 2,
	Installing: 3, Activating: 4, Registering: 5, Completed: 6,
}

type DownloadSnapshot struct {
	State     DownloadState `json:"state"`
	ErrorCode string        `json:"errorCode,omitempty"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

// CardDownloadService tracks one download operation. It does not replace the
// existing eSIM manager's network/install flow; the adapter advances it from
// real progress events. Failed is terminal and never silently restarts.
type CardDownloadService struct {
	mu       sync.RWMutex
	snapshot DownloadSnapshot
	plan     DownloadPlan
}

type DownloadPlan struct {
	AutoActivate bool
	RegisterIMS  bool
}

func NewCardDownloadService() *CardDownloadService {
	return NewCardDownloadServiceWithPlan(DownloadPlan{})
}

func NewCardDownloadServiceWithPlan(plan DownloadPlan) *CardDownloadService {
	return &CardDownloadService{
		snapshot: DownloadSnapshot{State: Preparing, UpdatedAt: time.Now().UTC()},
		plan:     plan,
	}
}

func (s *CardDownloadService) Snapshot() DownloadSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

func (s *CardDownloadService) Advance(next DownloadState, errorCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.snapshot.State
	if current == Failed || current == Completed {
		return errors.New("download already terminal")
	}
	if next == Failed {
		if errorCode == "" {
			return errors.New("failed download requires error code")
		}
	} else {
		if _, ok := downloadOrder[next]; !ok {
			return errors.New("unknown download state")
		}
		if downloadOrder[next] < downloadOrder[current] {
			return errors.New("download cannot move backward")
		}
		if next == Activating && !s.plan.AutoActivate {
			return errors.New("download plan does not activate")
		}
		if next == Registering && (!s.plan.RegisterIMS || !s.plan.AutoActivate) {
			return errors.New("download plan does not register IMS")
		}
		if next == Completed {
			required := Installing
			if s.plan.AutoActivate {
				required = Activating
			}
			if s.plan.RegisterIMS {
				required = Registering
			}
			if current != required {
				return errors.New("download cannot complete before planned stages")
			}
		}
		if errorCode != "" {
			return errors.New("error code only valid in FAILED state")
		}
	}
	s.snapshot = DownloadSnapshot{State: next, ErrorCode: errorCode, UpdatedAt: time.Now().UTC()}
	return nil
}
