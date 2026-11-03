// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// DefaultTaskStoreTTL is how long a stored task survives without being written again.
	// The control path is one request/response per call and reads the task back only at the
	// end of that same SendMessage, so a few minutes is ample; anything older is dead weight.
	DefaultTaskStoreTTL = 5 * time.Minute
	// DefaultTaskStoreMaxEntries is the hard cap on stored tasks. It bounds the store by
	// size as well as by age, so a burst of calls inside one TTL cannot grow it without limit.
	DefaultTaskStoreMaxEntries = 500
)

// TaskStoreEntriesMetricName is the gauge that reports how many tasks the store currently
// holds. It is what makes eviction observable on a running pod rather than only in a test.
const TaskStoreEntriesMetricName = "a2a_task_store_entries"

// BoundedTaskStoreConfig configures a BoundedTaskStore. Every field is optional; the zero
// value yields the defaults above with no owner scoping.
type BoundedTaskStoreConfig struct {
	// TTL is the age past which an entry is dropped. Zero means DefaultTaskStoreTTL.
	TTL time.Duration
	// MaxEntries is the hard cap. Zero means DefaultTaskStoreMaxEntries.
	MaxEntries int
	// Authenticator scopes tasks to an owner, as the SDK's own store does. Nil means no scoping.
	Authenticator taskstore.Authenticator
	// TimeProvider supplies the clock. Nil means time.Now; tests set it to advance the TTL.
	TimeProvider func() time.Time
	// Gauge reports the live entry count. Nil means the store creates and registers the
	// package gauge on the default registry.
	Gauge prometheus.Gauge
}

// BoundedTaskStore is a taskstore.Store with a TTL and a hard entry cap.
//
// The SDK's default store is a map with no delete, TTL or cap, so every control call pins
// its task — including the worker's bearer token in the CreateResponse artifact — for the
// life of the process. This store owns its map instead of wrapping the SDK's, because
// taskstore.Store exposes no Delete: eviction has to happen where the map lives.
//
// Eviction runs after every write: entries older than the TTL go first, then the
// least-recently-written entries are dropped until the cap holds.
type BoundedTaskStore struct {
	mu     sync.Mutex
	tasks  map[a2a.TaskID]*boundedStoredTask
	config BoundedTaskStoreConfig
	gauge  prometheus.Gauge
}

// boundedStoredTask is the stored form of a task. It mirrors the SDK's own storedTask so the
// version, ownership and recency semantics match the store it replaces.
type boundedStoredTask struct {
	task        *a2a.Task
	version     taskstore.TaskVersion
	user        string
	lastUpdated time.Time
}

var _ taskstore.Store = (*BoundedTaskStore)(nil)

// NewBoundedTaskStore builds a store from the given config, filling in the defaults.
func NewBoundedTaskStore(config BoundedTaskStoreConfig) *BoundedTaskStore {
	if config.TTL <= 0 {
		config.TTL = DefaultTaskStoreTTL
	}
	if config.MaxEntries <= 0 {
		config.MaxEntries = DefaultTaskStoreMaxEntries
	}
	if config.TimeProvider == nil {
		config.TimeProvider = time.Now
	}
	if config.Authenticator == nil {
		config.Authenticator = func(context.Context) (string, error) { return "", nil }
	}

	gauge := config.Gauge
	if gauge == nil {
		gauge = NewTaskStoreEntriesGauge()
	}

	store := &BoundedTaskStore{
		tasks:  make(map[a2a.TaskID]*boundedStoredTask),
		config: config,
		gauge:  gauge,
	}
	store.publishLen()
	return store
}

// NewTaskStoreEntriesGauge returns the process-wide entry-count gauge. A second call reuses
// the collector already registered rather than panicking, so tests can build many stores.
func NewTaskStoreEntriesGauge() prometheus.Gauge {
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: TaskStoreEntriesMetricName,
		Help: "Number of A2A tasks currently held by the bounded task store.",
	})
	if err := prometheus.Register(gauge); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := already.ExistingCollector.(prometheus.Gauge); ok {
				return existing
			}
		}
		panic(err)
	}
	return gauge
}

// Create implements taskstore.Store.
func (s *BoundedTaskStore) Create(
	ctx context.Context,
	task *a2a.Task,
) (taskstore.TaskVersion, error) {
	if err := validateBoundedTask(task); err != nil {
		return taskstore.TaskVersionMissing, err
	}

	userName, err := s.config.Authenticator(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore auth failed: %w", err)
	}

	taskCopy, err := deepCopyTask(task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tasks[task.ID] != nil {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}

	version := taskstore.TaskVersion(1)
	s.tasks[task.ID] = &boundedStoredTask{
		task:        taskCopy,
		version:     version,
		user:        userName,
		lastUpdated: s.config.TimeProvider(),
	}
	s.evictLocked()
	return version, nil
}

// Update implements taskstore.Store.
func (s *BoundedTaskStore) Update(
	ctx context.Context,
	req *taskstore.UpdateRequest,
) (taskstore.TaskVersion, error) {
	if err := validateBoundedTask(req.Task); err != nil {
		return taskstore.TaskVersionMissing, err
	}

	userName, err := s.config.Authenticator(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore auth failed: %w", err)
	}

	taskCopy, err := deepCopyTask(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	stored := s.tasks[req.Task.ID]
	if stored == nil || stored.user != userName {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if req.PrevVersion != taskstore.TaskVersionMissing && stored.version != req.PrevVersion {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}

	version := stored.version + 1
	s.tasks[req.Task.ID] = &boundedStoredTask{
		task:        taskCopy,
		version:     version,
		user:        stored.user,
		lastUpdated: s.config.TimeProvider(),
	}
	s.evictLocked()
	return version, nil
}

// Get implements taskstore.Store.
func (s *BoundedTaskStore) Get(
	ctx context.Context,
	taskID a2a.TaskID,
) (*taskstore.StoredTask, error) {
	s.mu.Lock()
	stored, ok := s.tasks[taskID]
	s.mu.Unlock()

	if !ok {
		return nil, a2a.ErrTaskNotFound
	}

	// Mask an ownership mismatch as not-found, as the SDK does and A2A §3.3.2 requires.
	userName, err := s.config.Authenticator(ctx)
	if err != nil {
		return nil, fmt.Errorf("taskstore auth failed: %w", err)
	}
	if stored.user != userName {
		return nil, a2a.ErrTaskNotFound
	}

	taskCopy, err := deepCopyTask(stored.task)
	if err != nil {
		return nil, fmt.Errorf("task copy failed: %w", err)
	}

	return &taskstore.StoredTask{Task: taskCopy, Version: stored.version, User: stored.user}, nil
}

// List implements taskstore.Store.
func (s *BoundedTaskStore) List(
	ctx context.Context,
	req *a2a.ListTasksRequest,
) (*a2a.ListTasksResponse, error) {
	const defaultPageSize = 50

	userName, err := s.config.Authenticator(ctx)
	if userName == "" || err != nil {
		return nil, a2a.ErrUnauthenticated
	}

	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	} else if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf(
			"page size must be between 1 and 100 inclusive, got %d: %w",
			pageSize,
			a2a.ErrInvalidRequest,
		)
	}

	s.mu.Lock()
	filtered := filterBoundedTasks(s.tasks, userName, req)
	s.mu.Unlock()

	totalSize := len(filtered)
	slices.SortFunc(filtered, func(a, b *boundedStoredTask) int {
		if timeCmp := b.lastUpdated.Compare(a.lastUpdated); timeCmp != 0 {
			return timeCmp
		}
		return strings.Compare(string(b.task.ID), string(a.task.ID))
	})

	page, nextPageToken, err := paginateBoundedTasks(filtered, pageSize, req)
	if err != nil {
		return nil, err
	}

	tasks, err := toBoundedListResult(page, req)
	if err != nil {
		return nil, err
	}

	return &a2a.ListTasksResponse{
		Tasks:         tasks,
		TotalSize:     totalSize,
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}, nil
}

// evictLocked drops expired entries, then the least-recently-written entries over the cap.
// The caller must hold s.mu.
func (s *BoundedTaskStore) evictLocked() {
	now := s.config.TimeProvider()

	for id, stored := range s.tasks {
		if now.Sub(stored.lastUpdated) >= s.config.TTL {
			delete(s.tasks, id)
		}
	}

	if over := len(s.tasks) - s.config.MaxEntries; over > 0 {
		s.evictOldestLocked(over)
	}

	s.publishLenLocked()
}

// evictOldestLocked removes the n least-recently-written entries. The caller must hold s.mu.
func (s *BoundedTaskStore) evictOldestLocked(n int) {
	ordered := make([]*boundedStoredTask, 0, len(s.tasks))
	for _, stored := range s.tasks {
		ordered = append(ordered, stored)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if cmp := ordered[i].lastUpdated.Compare(ordered[j].lastUpdated); cmp != 0 {
			return cmp < 0
		}
		return strings.Compare(string(ordered[i].task.ID), string(ordered[j].task.ID)) < 0
	})
	for _, stored := range ordered[:n] {
		delete(s.tasks, stored.task.ID)
	}
}

func (s *BoundedTaskStore) publishLen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLenLocked()
}

func (s *BoundedTaskStore) publishLenLocked() {
	if s.gauge != nil {
		s.gauge.Set(float64(len(s.tasks)))
	}
}

// Len reports how many tasks the store currently holds. It exists so a caller (and a test)
// can assert the bound without scraping the gauge.
func (s *BoundedTaskStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks)
}

func filterBoundedTasks(
	tasks map[a2a.TaskID]*boundedStoredTask,
	userName string,
	req *a2a.ListTasksRequest,
) []*boundedStoredTask {
	var filtered []*boundedStoredTask
	for _, stored := range tasks {
		if stored.user != userName {
			continue
		}
		if req.ContextID != "" && stored.task.ContextID != req.ContextID {
			continue
		}
		if req.Status != a2a.TaskStateUnspecified && stored.task.Status.State != req.Status {
			continue
		}
		if req.StatusTimestampAfter != nil && stored.task.Status.Timestamp != nil {
			if stored.task.Status.Timestamp.Before(*req.StatusTimestampAfter) {
				continue
			}
		}
		filtered = append(filtered, stored)
	}
	return filtered
}

func paginateBoundedTasks(
	filtered []*boundedStoredTask,
	pageSize int,
	req *a2a.ListTasksRequest,
) ([]*boundedStoredTask, string, error) {
	var cursorTime time.Time
	var cursorTaskID a2a.TaskID

	var page []*boundedStoredTask
	if req.PageToken != "" {
		var err error
		cursorTime, cursorTaskID, err = decodeBoundedPageToken(req.PageToken)
		if err != nil {
			return nil, "", err
		}
		start := sort.Search(len(filtered), func(i int) bool {
			task := filtered[i]
			if cmp := task.lastUpdated.Compare(cursorTime); cmp != 0 {
				return cmp < 0
			}
			return strings.Compare(string(task.task.ID), string(cursorTaskID)) < 0
		})
		page = filtered[start:]
	} else {
		page = filtered
	}

	var nextPageToken string
	if pageSize >= len(page) {
		pageSize = len(page)
	} else {
		last := page[pageSize-1]
		nextPageToken = encodeBoundedPageToken(last.lastUpdated, last.task.ID)
	}
	return page[:pageSize], nextPageToken, nil
}

func toBoundedListResult(
	tasks []*boundedStoredTask,
	req *a2a.ListTasksRequest,
) ([]*a2a.Task, error) {
	const defaultMaxHistoryLength = 100

	var result []*a2a.Task
	for _, stored := range tasks {
		taskCopy, err := deepCopyTask(stored.task)
		if err != nil {
			return nil, err
		}
		historyLength := defaultMaxHistoryLength
		if req.HistoryLength != nil {
			historyLength = *req.HistoryLength
		}
		if historyLength <= 0 {
			taskCopy.History = []*a2a.Message{}
		} else if len(taskCopy.History) > historyLength {
			taskCopy.History = taskCopy.History[len(taskCopy.History)-historyLength:]
		}
		if !req.IncludeArtifacts {
			taskCopy.Artifacts = nil
		}
		result = append(result, taskCopy)
	}
	return result, nil
}

func encodeBoundedPageToken(updated time.Time, taskID a2a.TaskID) string {
	return base64.URLEncoding.EncodeToString(
		fmt.Appendf(nil, "%s_%s", updated.Format(time.RFC3339Nano), taskID),
	)
}

func decodeBoundedPageToken(token string) (time.Time, a2a.TaskID, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	parts := strings.Split(string(decoded), "_")
	if len(parts) != 2 {
		return time.Time{}, "", a2a.ErrParseError
	}
	updated, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	return updated, a2a.TaskID(parts[1]), nil
}

// deepCopyTask copies a task through gob, which is how the SDK's own store copies it. The
// artifact payload types must be registered for this to succeed; *ControlResponse is, in
// gob.go.
func deepCopyTask(task *a2a.Task) (*a2a.Task, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(*task); err != nil {
		return nil, err
	}
	var copied a2a.Task
	if err := gob.NewDecoder(&buf).Decode(&copied); err != nil {
		return nil, err
	}
	return &copied, nil
}

// validateBoundedTask mirrors the SDK's own task validation, which is unexported. Skipping
// it would make this store accept a task the SDK's store rejects, which is a behaviour
// change in exactly the dimension the bounded store must preserve.
func validateBoundedTask(task *a2a.Task) error {
	if task == nil {
		return nil
	}
	if err := validateBoundedMessage(task.Status.Message); err != nil {
		return err
	}
	for _, msg := range task.History {
		if err := validateBoundedMessage(msg); err != nil {
			return err
		}
	}
	for _, artifact := range task.Artifacts {
		if err := validateBoundedArtifact(artifact); err != nil {
			return err
		}
	}
	return validateBoundedMeta(task.Metadata)
}

func validateBoundedArtifact(artifact *a2a.Artifact) error {
	if artifact == nil {
		return nil
	}
	if err := validateBoundedParts(artifact.Parts); err != nil {
		return err
	}
	return validateBoundedMeta(artifact.Metadata)
}

func validateBoundedMessage(msg *a2a.Message) error {
	if msg == nil {
		return nil
	}
	if err := validateBoundedParts(msg.Parts); err != nil {
		return err
	}
	return validateBoundedMeta(msg.Metadata)
}

func validateBoundedParts(parts a2a.ContentParts) error {
	for _, part := range parts {
		if err := validateBoundedMeta(part.Meta()); err != nil {
			return err
		}
	}
	return nil
}

func validateBoundedMeta(meta map[string]any) error {
	return validateBoundedMetaRecursive(meta, map[string]struct{}{})
}

func validateBoundedMetaRecursive(value any, processing map[string]struct{}) error {
	if value == nil {
		return nil
	}

	// uint is excluded: unsigned types do not play well with the spec's JSON mapping.
	switch value.(type) {
	case bool, int, int8, int16, int32, int64, float32, float64, string:
		return nil
	}

	key := fmt.Sprintf("%p", value)
	if _, ok := processing[key]; ok {
		return fmt.Errorf("circular reference in Metadata")
	}
	processing[key] = struct{}{}
	defer delete(processing, key)

	if arr, ok := value.([]any); ok {
		for _, elem := range arr {
			if err := validateBoundedMetaRecursive(elem, processing); err != nil {
				return err
			}
		}
		return nil
	}

	if m, ok := value.(map[string]any); ok {
		for _, elem := range m {
			if err := validateBoundedMetaRecursive(elem, processing); err != nil {
				return err
			}
		}
		return nil
	}

	return fmt.Errorf(
		"%T is not permitted in Metadata, must be one of nil, bool, int, float, string, []any, map[string]any",
		value,
	)
}
