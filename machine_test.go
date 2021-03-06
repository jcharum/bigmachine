// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package bigmachine

import (
	"bytes"
	"context"
	"encoding/gob"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/grailbio/base/errors"
	"github.com/grailbio/bigmachine/rpc"
)

var fakeDigest = digester.FromString("fake binary")

type fakeSupervisor struct {
	Args          []string
	Environ       []string
	Image         []byte
	LastKeepalive time.Time
	Hung          bool
	Execd         bool
}

func (s *fakeSupervisor) Setenv(ctx context.Context, env []string, _ *struct{}) error {
	s.Environ = env
	return nil
}

func (s *fakeSupervisor) Setargs(ctx context.Context, args []string, _ *struct{}) error {
	s.Args = args
	return nil
}

func (s *fakeSupervisor) Setbinary(ctx context.Context, binary io.Reader, _ *struct{}) (err error) {
	s.Image, err = ioutil.ReadAll(binary)
	return err
}

func (s *fakeSupervisor) GetBinary(ctx context.Context, _ struct{}, rc *io.ReadCloser) error {
	if s.Image == nil {
		return errors.E(errors.Invalid, "no binary set")
	}
	*rc = ioutil.NopCloser(bytes.NewReader(s.Image))
	return nil
}

func (s *fakeSupervisor) Exec(ctx context.Context, exec io.Reader, _ *struct{}) error {
	s.Execd = true
	return nil
}

func (s *fakeSupervisor) Tail(ctx context.Context, fd int, rc *io.ReadCloser) error {
	return errors.New("not supported")
}

func (s *fakeSupervisor) Ping(ctx context.Context, seq int, replyseq *int) error {
	*replyseq = seq
	return nil
}

func (s *fakeSupervisor) Info(ctx context.Context, _ struct{}, info *Info) error {
	info.Goos = runtime.GOOS
	info.Goarch = runtime.GOARCH
	info.Digest = fakeDigest
	return nil
}

func (s *fakeSupervisor) Keepalive(ctx context.Context, next time.Duration, reply *keepaliveReply) error {
	if s.Hung {
		<-ctx.Done()
		return ctx.Err()
	}
	s.LastKeepalive = time.Now()
	reply.Next = next
	reply.Healthy = true
	return nil
}

func (s *fakeSupervisor) Hang(ctx context.Context, _ struct{}, _ *struct{}) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *fakeSupervisor) Register(ctx context.Context, svc service, _ *struct{}) error {
	// Tests only require that we Init services (if needed), so we don't do any
	// actual registration.
	return maybeInit(svc.Instance, nil)
}

func newTestMachine(t *testing.T, params ...Param) (m *Machine, supervisor *fakeSupervisor, shutdown func()) {
	t.Helper()
	supervisor = new(fakeSupervisor)
	srv := rpc.NewServer()
	if err := srv.Register("Supervisor", supervisor); err != nil {
		t.Fatal(err)
	}
	httpsrv := httptest.NewServer(srv)
	client, err := rpc.NewClient(func() *http.Client { return httpsrv.Client() }, "/")
	if err != nil {
		httpsrv.Close()
		t.Fatal(err)
	}
	m = &Machine{
		Addr:                httpsrv.URL,
		client:              client,
		owner:               true,
		keepalivePeriod:     time.Minute,
		keepaliveTimeout:    2 * time.Minute,
		keepaliveRpcTimeout: 10 * time.Second,
		tailDone:            make(chan struct{}),
	}
	for _, param := range params {
		param.applyParam(m)
	}
	m.start(nil)
	return m, supervisor, func() {
		m.Cancel()
		select {
		case <-m.Wait(Stopped):
		case <-time.After(time.Second):
			t.Log("failed to stop server after 1 second")
		}
		httpsrv.Close()
	}
}

func TestMachineBootup(t *testing.T) {
	m, supervisor, shutdown := newTestMachine(t)
	defer shutdown()

	<-m.Wait(Running)
	if got, want := m.State(), Running; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	r, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	image, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(supervisor.Image, image) {
		t.Error("image does not match")
	}
	if time.Since(supervisor.LastKeepalive) > time.Minute {
		t.Errorf("failed to maintain keepalive")
	}
}

func TestMachineEnv(t *testing.T) {
	m, supervisor, shutdown := newTestMachine(t, Environ{"test=yes"})
	defer shutdown()
	<-m.Wait(Running)
	if got, want := len(supervisor.Environ), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := supervisor.Environ[0], "test=yes"; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCallTimeout(t *testing.T) {
	m, _, shutdown := newTestMachine(t)
	defer shutdown()
	const timeout = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err := m.Call(ctx, "Supervisor.Hang", struct{}{}, nil)
	if got, want := err, context.DeadlineExceeded; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	cancel()
}

func TestMachineContext(t *testing.T) {
	log.SetFlags(log.Llongfile)
	m, supervisor, shutdown := newTestMachine(t)
	defer shutdown()
	supervisor.Hung = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Second)
		cancel()
	}()
	err := m.Call(ctx, "Supervisor.Hang", struct{}{}, nil)
	if got, want := err, context.Canceled; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// serviceGobUnregistered is a service that is not registered with gob, so
// attempts to register it will fail.
type serviceGobUnregistered struct{}

// TestServiceGobUnregisteredFastFail verifies that we fail fast when a service
// is not gob-encodable.
func TestServiceGobUnregisteredFastFail(t *testing.T) {
	m, _, shutdown := newTestMachine(t, Services{"GobUnregistered": serviceGobUnregistered{}})
	defer shutdown()
	select {
	case <-m.Wait(Running):
		if m.State() == Running {
			t.Fatalf("machine is running with broken service")
		}
	case <-time.After(2 * time.Minute):
		// If our test environment causes this to falsely fail, we almost
		// surely have lots of other problems, as this should otherwise fail
		// almost instantly.
		t.Fatalf("took too long to fail")
	}
}

// serviceInitPanic is a service that panics in Init, indicating that the
// service is fatally broken.
type serviceInitPanic struct{}

func (serviceInitPanic) Init(b *B) error {
	panic("")
}

func init() {
	gob.Register(serviceInitPanic{})
}

// TestBadServiceFastFail verifies that we fail fast when a service panics in
// its Init.
func TestServiceInitPanicFastFail(t *testing.T) {
	m, _, shutdown := newTestMachine(t, Services{"InitPanic": serviceInitPanic{}})
	defer shutdown()
	select {
	case <-m.Wait(Running):
		if m.State() == Running {
			t.Fatalf("machine is running with broken service")
		}
	case <-time.After(2 * time.Minute):
		// If our test environment causes this to falsely fail, we almost
		// surely have lots of other problems, as this should otherwise fail
		// almost instantly.
		t.Fatalf("took too long to fail")
	}
}
