// Copyright (c) 2025 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package handler

import (
	"net/http"

	libhttp "github.com/bborbe/http"
	"github.com/golang/glog"
)

// NewTestLoglevelHandler creates an HTTP handler that tests different glog verbosity levels.
// It logs messages at various levels to verify logging configuration.
func NewTestLoglevelHandler() http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		glog.Errorf("error")
		glog.Warningf("warn")
		glog.V(0).Infof("info 0")
		glog.V(1).Infof("info 1")
		glog.V(2).Infof("info 2")
		glog.V(3).Infof("info 3")
		glog.V(4).Infof("info 4")
		_, _ = libhttp.WriteAndGlog(resp, "test loglevel completed")
	})
}
