// Copyright 2019 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grailbio/base/errors"
)

func TestNetError(t *testing.T) {
	srv := NewServer()
	srv.Register("Test", new(TestService))
	httpsrv := httptest.NewServer(srv)
	client, err := NewClient(func() *http.Client { return httpsrv.Client() }, testPrefix)
	if err != nil {
		t.Fatal(err)
	}
	e := errors.E(errors.Net, "some network error")
	err = client.Call(context.Background(), httpsrv.URL, "Test.ErrorError", e, nil)
	if err == nil {
		t.Error("expected error")
	} else if errors.Is(errors.Net, err) {
		t.Errorf("error %v is a network error", err)
	} else if got, want := err.Error(), "some network error"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}