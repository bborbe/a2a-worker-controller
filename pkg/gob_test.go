// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"bytes"
	"encoding/gob"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// carrier mirrors the shape the artifact actually travels in. The SDK stores it as
// `a2a.Data{Value: any}` — the value sits in an **interface-typed struct field**, which gob
// encodes with a registered-type-name indirection.
//
// The field shape is load-bearing and was got wrong once here: encoding the value at the
// TOP level instead lets gob flatten the pointer and write a concrete type, so the decode
// fails with "local interface type *interface {} can only be decoded from remote interface
// type" — a different error that has nothing to do with registration and would have read as
// "the fix does not work".
type carrier struct {
	Value any
}

// Without the registration in pkg/gob.go this fails with the literal
// `gob: type not registered for interface: pkg.ControlResponse` — the error that made every
// control call return TASK_STATE_FAILED on the running binary while 49 specs stayed green,
// because those specs drive the executor directly and never cross this boundary.
//
// It pins the POINTER, because that is what the executor stores: dispatch returns
// (*ControlResponse, error) and a2a.NewDataPart puts it in Data{Value: any}. A registration
// of the value type would leave this failing.
var _ = Describe("ControlResponse gob round-trip", func() {
	It("survives the copy the task store performs on every write", func() {
		var buf bytes.Buffer
		Expect(gob.NewEncoder(&buf).Encode(carrier{Value: &pkg.ControlResponse{}})).To(Succeed())

		var decoded carrier
		Expect(gob.NewDecoder(&buf).Decode(&decoded)).To(Succeed())
		Expect(decoded.Value).To(BeAssignableToTypeOf(&pkg.ControlResponse{}))
	})
})
