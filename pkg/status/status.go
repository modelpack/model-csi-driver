package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/trace"
)

type State = string

const (
	StatePullRunning   = "PULLING"
	StatePullSucceeded = "PULL_SUCCEEDED"
	StatePullFailed    = "PULL_FAILED"
	StatePullTimeout   = "PULL_TIMEOUT"
	StatePullCanceled  = "PULL_CANCELED"
	StateMounted       = "MOUNTED"
	StateUmounted      = "UMOUNTED"
)

type StatusManager struct {
	mutex sync.Mutex
}

type ProgressItem struct {
	Digest    digest.Digest `json:"digest"`
	Path      string        `json:"path"`
	Size      int64         `json:"size"`
	StartedAt time.Time     `json:"started_at"`

	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      error      `json:"error,omitempty"`

	Span trace.Span `json:"-"`
}

type Progress struct {
	Total int            `json:"total"`
	Items []ProgressItem `json:"items"`
}

func (p *Progress) String() (string, error) {
	progressBytes, err := json.Marshal(p)
	if err != nil {
		return "", errors.Wrap(err, "marshal progress")
	}
	return string(progressBytes), nil
}

type Status struct {
	VolumeName string   `json:"volume_name,omitempty"`
	MountID    string   `json:"mount_id,omitempty"`
	Reference  string   `json:"reference,omitempty"`
	State      State    `json:"state,omitempty"`
	Inline     bool     `json:"inline,omitempty"`
	Progress   Progress `json:"progress,omitempty"`
}

func NewStatusManager() (*StatusManager, error) {
	return &StatusManager{}, nil
}

func (sm *StatusManager) set(statusPath string, status Status) (*Status, error) {
	volumeStatusDir := filepath.Dir(statusPath)
	if err := os.MkdirAll(volumeStatusDir, 0755); err != nil {
		return nil, errors.Wrap(err, "create status dir")
	}

	statusBytes, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return nil, errors.Wrap(err, "marshal status")
	}

	if err := os.WriteFile(statusPath, statusBytes, 0644); err != nil {
		return nil, errors.Wrap(err, "write status file")
	}

	return &status, nil
}

func (sm *StatusManager) get(statusPath string) (*Status, error) {
	statusBytes, err := os.ReadFile(statusPath)
	if err != nil {
		return nil, errors.Wrap(err, "read status file")
	}

	if strings.TrimSpace(string(statusBytes)) == "" {
		return nil, errors.Wrap(os.ErrNotExist, "status file is empty")
	}

	status := Status{}
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		return nil, errors.Wrap(os.ErrNotExist, "unmarshal status file")
	}

	return &status, nil
}

func (sm *StatusManager) getWithLock(statusPath string) (*Status, error) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	status, err := sm.get(statusPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrapf(os.ErrNotExist, "status not found: %s", statusPath)
		}
		return nil, errors.Wrapf(err, "get status: %s", statusPath)
	}

	return status, nil
}

func (sm *StatusManager) Set(statusPath string, newStatus Status) (*Status, error) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	status, err := sm.set(statusPath, newStatus)
	if err != nil {
		return nil, errors.Wrapf(err, "create new status: %s", statusPath)
	}
	return status, nil
}

func (sm *StatusManager) Get(statusPath string) (*Status, error) {
	status, err := sm.getWithLock(statusPath)
	if err != nil {
		return nil, err
	}

	return status, nil
}
