// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// listBody is a control call that starts no worker: the operation is a read, but the SDK
// still mints and stores one A2A task for the SendMessage itself. That is the exact shape
// the leak takes, so it is what the bound is measured against.
const listBody = `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":` +
	`{"messageId":"m1","role":"ROLE_USER","parts":[{"data":{"operation":"list"}}]}}}`

// createBody starts a stub worker — no pod, and the stub derives the worker id and token from
// the task name, so both runs of the comparison produce the same values.
const createBody = `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":` +
	`{"messageId":"m1","role":"ROLE_USER","parts":[{"data":{"operation":"create",` +
	`"create":{"task":"equality-task"}}}]}}}`

const stopBody = `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":` +
	`{"messageId":"m1","role":"ROLE_USER","parts":[{"data":{"operation":"stop",` +
	`"stop":{"worker_id":"equality-task"}}}]}}}`

var (
	// The SDK mints a fresh task id, artifact id and context id per call.
	uuidPattern = regexp.MustCompile(
		`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`,
	)
	// The handler stamps the completion time, which differs between two runs.
	timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z`)
)

// normalize masks the two values the SDK and the clock produce per call, leaving everything
// the bounded store is responsible for untouched.
func normalize(body string) string {
	return timestampPattern.ReplaceAllString(
		uuidPattern.ReplaceAllString(body, "<task-id>"),
		"<timestamp>",
	)
}

func newExecutor() a2asrv.AgentExecutor {
	return pkg.NewA2AExecutor(
		pkg.NewBackendProvider(pkg.NewStubWorkerBackend(), nil, nil),
		pkg.BackendDocker,
	)
}

func send(handler http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", pkg.A2APath, strings.NewReader(body))
	req.Header.Set("A2A-Version", string(a2a.Version))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func task(id a2a.TaskID) *a2a.Task {
	return &a2a.Task{
		ID:     id,
		Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
}

var _ = Describe("BoundedTaskStore", func() {
	It("returns the same create, list and stop responses as the SDK's unbounded store", func() {
		bounded := pkg.NewA2AHandler(
			newExecutor(),
			pkg.NewBoundedTaskStore(pkg.BoundedTaskStoreConfig{}),
		)
		unbounded := pkg.NewA2AHandler(newExecutor(), taskstore.NewInMemory(nil))

		for _, body := range []string{createBody, listBody, stopBody} {
			boundedBody := send(bounded, body).Body.String()
			unboundedBody := send(unbounded, body).Body.String()

			Expect(boundedBody).NotTo(ContainSubstring(`TASK_STATE_FAILED`))
			// The only differences are the per-call ids and the completion timestamp.
			Expect(normalize(boundedBody)).To(Equal(normalize(unboundedBody)))
		}
	})

	It("drops the least-recently-written entry once the cap is exceeded", func() {
		store := pkg.NewBoundedTaskStore(pkg.BoundedTaskStoreConfig{
			MaxEntries: 3,
			TTL:        time.Hour,
		})
		ctx := context.Background()

		for i := 0; i < 4; i++ {
			_, err := store.Create(ctx, task(a2a.TaskID(fmt.Sprintf("task-%d", i))))
			Expect(err).To(BeNil())
		}

		Expect(store.Len()).To(Equal(3))
		_, err := store.Get(ctx, "task-0")
		Expect(err).To(MatchError(a2a.ErrTaskNotFound))
		_, err = store.Get(ctx, "task-3")
		Expect(err).To(BeNil())
	})

	It("drops an entry once it is older than the TTL", func() {
		now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		store := pkg.NewBoundedTaskStore(pkg.BoundedTaskStoreConfig{
			MaxEntries:   500,
			TTL:          5 * time.Minute,
			TimeProvider: func() time.Time { return now },
		})
		ctx := context.Background()

		_, err := store.Create(ctx, task("old"))
		Expect(err).To(BeNil())

		now = now.Add(6 * time.Minute)

		_, err = store.Create(ctx, task("new"))
		Expect(err).To(BeNil())

		Expect(store.Len()).To(Equal(1))
		_, err = store.Get(ctx, "old")
		Expect(err).To(MatchError(a2a.ErrTaskNotFound))
		_, err = store.Get(ctx, "new")
		Expect(err).To(BeNil())
	})

	It("holds at most the cap after 10,000 control calls", func() {
		store := pkg.NewBoundedTaskStore(pkg.BoundedTaskStoreConfig{})
		handler := pkg.NewA2AHandler(newExecutor(), store)

		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		for i := 0; i < 10000; i++ {
			Expect(send(handler, listBody).Code).To(Equal(http.StatusOK))
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)

		Expect(store.Len()).To(BeNumerically("<=", pkg.DefaultTaskStoreMaxEntries))
		Expect(after.HeapAlloc).To(BeNumerically("<", before.HeapAlloc+5*1024*1024))
	})
})
