// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import "encoding/gob"

// Registering the artifact type with gob is what makes the control API work at all.
//
// The A2A SDK's task store copies every task through encoding/gob before writing it —
// `a2asrv/taskstore/inmemory.go`, whose deep copy is a gob encoder over the task — and it
// registers only the two collection types it needs itself, `map[string]any` and `[]any`,
// never the caller's payload. An artifact type that is not registered therefore fails that
// copy **after** the executor has already done its work: the caller sees a task in
// `TASK_STATE_FAILED` while the operation's side effects are already applied, which is why
// a failed status must never be read as "the operation did not happen".
//
// The POINTER is what must be registered, not the value. `dispatch` returns
// `(*ControlResponse, error)` and `a2a.NewDataPart` stores it in `Data{Value: any}`, so the
// dynamic type sitting in that interface is `*ControlResponse`. Registering
// `ControlResponse` instead leaves the pointer unregistered and the copy fails exactly as
// before — which reads as "the diagnosis was wrong" rather than as a fix that missed by one
// character. `gob_test.go` pins the pointer form so a refactor between pointer and value
// cannot silently reopen this.
func init() {
	gob.Register(&ControlResponse{})
}
