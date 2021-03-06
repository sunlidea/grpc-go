/*
 *
 * Copyright 2018 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/balancerload"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"
	testpb "google.golang.org/grpc/test/grpc_testing"
	"google.golang.org/grpc/testdata"
)

const testBalancerName = "testbalancer"

// testBalancer creates one subconn with the first address from resolved
// addresses.
//
// It's used to test options for NewSubConn are applied correctly.
type testBalancer struct {
	cc balancer.ClientConn
	sc balancer.SubConn

	newSubConnOptions balancer.NewSubConnOptions
	pickInfos         []balancer.PickInfo
	doneInfo          []balancer.DoneInfo
}

func (b *testBalancer) Build(cc balancer.ClientConn, opt balancer.BuildOptions) balancer.Balancer {
	b.cc = cc
	return b
}

func (*testBalancer) Name() string {
	return testBalancerName
}

func (*testBalancer) ResolverError(err error) {
	panic("not implemented")
}

func (b *testBalancer) UpdateClientConnState(state balancer.ClientConnState) error {
	// Only create a subconn at the first time.
	if b.sc == nil {
		var err error
		b.sc, err = b.cc.NewSubConn(state.ResolverState.Addresses, b.newSubConnOptions)
		if err != nil {
			grpclog.Errorf("testBalancer: failed to NewSubConn: %v", err)
			return nil
		}
		b.cc.UpdateState(balancer.State{ConnectivityState: connectivity.Connecting, Picker: &picker{sc: b.sc, bal: b}})
		b.sc.Connect()
	}
	return nil
}

func (b *testBalancer) UpdateSubConnState(sc balancer.SubConn, s balancer.SubConnState) {
	grpclog.Infof("testBalancer: UpdateSubConnState: %p, %v", sc, s)
	if b.sc != sc {
		grpclog.Infof("testBalancer: ignored state change because sc is not recognized")
		return
	}
	if s.ConnectivityState == connectivity.Shutdown {
		b.sc = nil
		return
	}

	switch s.ConnectivityState {
	case connectivity.Ready, connectivity.Idle:
		b.cc.UpdateState(balancer.State{ConnectivityState: s.ConnectivityState, Picker: &picker{sc: sc, bal: b}})
	case connectivity.Connecting:
		b.cc.UpdateState(balancer.State{ConnectivityState: s.ConnectivityState, Picker: &picker{err: balancer.ErrNoSubConnAvailable, bal: b}})
	case connectivity.TransientFailure:
		b.cc.UpdateState(balancer.State{ConnectivityState: s.ConnectivityState, Picker: &picker{err: balancer.ErrTransientFailure, bal: b}})
	}
}

func (b *testBalancer) Close() {
}

type picker struct {
	err error
	sc  balancer.SubConn
	bal *testBalancer
}

func (p *picker) Pick(info balancer.PickInfo) (balancer.PickResult, error) {
	if p.err != nil {
		return balancer.PickResult{}, p.err
	}
	info.Ctx = nil // Do not validate context.
	p.bal.pickInfos = append(p.bal.pickInfos, info)
	return balancer.PickResult{SubConn: p.sc, Done: func(d balancer.DoneInfo) { p.bal.doneInfo = append(p.bal.doneInfo, d) }}, nil
}

func (s) TestCredsBundleFromBalancer(t *testing.T) {
	balancer.Register(&testBalancer{
		newSubConnOptions: balancer.NewSubConnOptions{
			CredsBundle: &testCredsBundle{},
		},
	})
	te := newTest(t, env{name: "creds-bundle", network: "tcp", balancer: ""})
	te.tapHandle = authHandle
	te.customDialOptions = []grpc.DialOption{
		grpc.WithBalancerName(testBalancerName),
	}
	creds, err := credentials.NewServerTLSFromFile(testdata.Path("server1.pem"), testdata.Path("server1.key"))
	if err != nil {
		t.Fatalf("Failed to generate credentials %v", err)
	}
	te.customServerOptions = []grpc.ServerOption{
		grpc.Creds(creds),
	}
	te.startServer(&testServer{})
	defer te.tearDown()

	cc := te.clientConn()
	tc := testpb.NewTestServiceClient(cc)
	if _, err := tc.EmptyCall(context.Background(), &testpb.Empty{}); err != nil {
		t.Fatalf("Test failed. Reason: %v", err)
	}
}

func (s) TestDoneInfo(t *testing.T) {
	for _, e := range listTestEnv() {
		testDoneInfo(t, e)
	}
}

func testDoneInfo(t *testing.T, e env) {
	te := newTest(t, e)
	b := &testBalancer{}
	balancer.Register(b)
	te.customDialOptions = []grpc.DialOption{
		grpc.WithBalancerName(testBalancerName),
	}
	te.userAgent = failAppUA
	te.startServer(&testServer{security: e.security})
	defer te.tearDown()

	cc := te.clientConn()
	tc := testpb.NewTestServiceClient(cc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wantErr := detailedError
	if _, err := tc.EmptyCall(ctx, &testpb.Empty{}); !testutils.StatusErrEqual(err, wantErr) {
		t.Fatalf("TestService/EmptyCall(_, _) = _, %v, want _, %v", err, wantErr)
	}
	if _, err := tc.UnaryCall(ctx, &testpb.SimpleRequest{}); err != nil {
		t.Fatalf("TestService.UnaryCall(%v, _, _, _) = _, %v; want _, <nil>", ctx, err)
	}

	if len(b.doneInfo) < 1 || !testutils.StatusErrEqual(b.doneInfo[0].Err, wantErr) {
		t.Fatalf("b.doneInfo = %v; want b.doneInfo[0].Err = %v", b.doneInfo, wantErr)
	}
	if len(b.doneInfo) < 2 || !reflect.DeepEqual(b.doneInfo[1].Trailer, testTrailerMetadata) {
		t.Fatalf("b.doneInfo = %v; want b.doneInfo[1].Trailer = %v", b.doneInfo, testTrailerMetadata)
	}
	if len(b.pickInfos) != len(b.doneInfo) {
		t.Fatalf("Got %d picks, but %d doneInfo, want equal amount", len(b.pickInfos), len(b.doneInfo))
	}
	// To test done() is always called, even if it's returned with a non-Ready
	// SubConn.
	//
	// Stop server and at the same time send RPCs. There are chances that picker
	// is not updated in time, causing a non-Ready SubConn to be returned.
	finished := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			tc.UnaryCall(ctx, &testpb.SimpleRequest{})
		}
		close(finished)
	}()
	te.srv.Stop()
	<-finished
	if len(b.pickInfos) != len(b.doneInfo) {
		t.Fatalf("Got %d picks, %d doneInfo, want equal amount", len(b.pickInfos), len(b.doneInfo))
	}
}

const loadMDKey = "X-Endpoint-Load-Metrics-Bin"

type testLoadParser struct{}

func (*testLoadParser) Parse(md metadata.MD) interface{} {
	vs := md.Get(loadMDKey)
	if len(vs) == 0 {
		return nil
	}
	return vs[0]
}

func init() {
	balancerload.SetParser(&testLoadParser{})
}

func (s) TestDoneLoads(t *testing.T) {
	for _, e := range listTestEnv() {
		testDoneLoads(t, e)
	}
}

func testDoneLoads(t *testing.T, e env) {
	b := &testBalancer{}
	balancer.Register(b)

	const testLoad = "test-load-,-should-be-orca"

	ss := &stubServer{
		emptyCall: func(ctx context.Context, in *testpb.Empty) (*testpb.Empty, error) {
			grpc.SetTrailer(ctx, metadata.Pairs(loadMDKey, testLoad))
			return &testpb.Empty{}, nil
		},
	}
	if err := ss.Start(nil, grpc.WithBalancerName(testBalancerName)); err != nil {
		t.Fatalf("error starting testing server: %v", err)
	}
	defer ss.Stop()

	tc := testpb.NewTestServiceClient(ss.cc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := tc.EmptyCall(ctx, &testpb.Empty{}); err != nil {
		t.Fatalf("TestService/EmptyCall(_, _) = _, %v, want _, %v", err, nil)
	}

	piWant := []balancer.PickInfo{
		{FullMethodName: "/grpc.testing.TestService/EmptyCall"},
	}
	if !reflect.DeepEqual(b.pickInfos, piWant) {
		t.Fatalf("b.pickInfos = %v; want %v", b.pickInfos, piWant)
	}

	if len(b.doneInfo) < 1 {
		t.Fatalf("b.doneInfo = %v, want length 1", b.doneInfo)
	}
	gotLoad, _ := b.doneInfo[0].ServerLoad.(string)
	if gotLoad != testLoad {
		t.Fatalf("b.doneInfo[0].ServerLoad = %v; want = %v", b.doneInfo[0].ServerLoad, testLoad)
	}
}

const testBalancerKeepAddressesName = "testbalancer-keepingaddresses"

// testBalancerKeepAddresses keeps the addresses in the builder instead of
// creating SubConns.
//
// It's used to test the addresses balancer gets are correct.
type testBalancerKeepAddresses struct {
	addrsChan chan []resolver.Address
}

func newTestBalancerKeepAddresses() *testBalancerKeepAddresses {
	return &testBalancerKeepAddresses{
		addrsChan: make(chan []resolver.Address, 10),
	}
}

func (testBalancerKeepAddresses) ResolverError(err error) {
	panic("not implemented")
}

func (b *testBalancerKeepAddresses) Build(cc balancer.ClientConn, opt balancer.BuildOptions) balancer.Balancer {
	return b
}

func (*testBalancerKeepAddresses) Name() string {
	return testBalancerKeepAddressesName
}

func (b *testBalancerKeepAddresses) UpdateClientConnState(state balancer.ClientConnState) error {
	b.addrsChan <- state.ResolverState.Addresses
	return nil
}

func (testBalancerKeepAddresses) UpdateSubConnState(sc balancer.SubConn, s balancer.SubConnState) {
	panic("not used")
}

func (testBalancerKeepAddresses) Close() {
}

// Make sure that non-grpclb balancers don't get grpclb addresses even if name
// resolver sends them
func (s) TestNonGRPCLBBalancerGetsNoGRPCLBAddress(t *testing.T) {
	r, rcleanup := manual.GenerateAndRegisterManualResolver()
	defer rcleanup()

	b := newTestBalancerKeepAddresses()
	balancer.Register(b)

	cc, err := grpc.Dial(r.Scheme()+":///test.server", grpc.WithInsecure(),
		grpc.WithBalancerName(b.Name()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer cc.Close()

	grpclbAddresses := []resolver.Address{{
		Addr:       "grpc.lb.com",
		Type:       resolver.GRPCLB,
		ServerName: "grpc.lb.com",
	}}

	nonGRPCLBAddresses := []resolver.Address{{
		Addr: "localhost",
		Type: resolver.Backend,
	}}

	r.UpdateState(resolver.State{
		Addresses: nonGRPCLBAddresses,
	})
	if got := <-b.addrsChan; !reflect.DeepEqual(got, nonGRPCLBAddresses) {
		t.Fatalf("With only backend addresses, balancer got addresses %v, want %v", got, nonGRPCLBAddresses)
	}

	r.UpdateState(resolver.State{
		Addresses: grpclbAddresses,
	})
	if got := <-b.addrsChan; len(got) != 0 {
		t.Fatalf("With only grpclb addresses, balancer got addresses %v, want empty", got)
	}

	r.UpdateState(resolver.State{
		Addresses: append(grpclbAddresses, nonGRPCLBAddresses...),
	})
	if got := <-b.addrsChan; !reflect.DeepEqual(got, nonGRPCLBAddresses) {
		t.Fatalf("With both backend and grpclb addresses, balancer got addresses %v, want %v", got, nonGRPCLBAddresses)
	}
}

// TestServersSwap creates two servers and verifies the client switches between
// them when the name resolver reports the first and then the second.
func (s) TestServersSwap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Initialize servers
	reg := func(username string) (addr string, cleanup func()) {
		lis, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatalf("Error while listening. Err: %v", err)
		}
		s := grpc.NewServer()
		ts := &funcServer{
			unaryCall: func(ctx context.Context, in *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
				return &testpb.SimpleResponse{Username: username}, nil
			},
		}
		testpb.RegisterTestServiceServer(s, ts)
		go s.Serve(lis)
		return lis.Addr().String(), s.Stop
	}
	const one = "1"
	addr1, cleanup := reg(one)
	defer cleanup()
	const two = "2"
	addr2, cleanup := reg(two)
	defer cleanup()

	// Initialize client
	r, cleanup := manual.GenerateAndRegisterManualResolver()
	defer cleanup()
	r.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: addr1}}})
	cc, err := grpc.DialContext(ctx, r.Scheme()+":///", grpc.WithInsecure())
	if err != nil {
		t.Fatalf("Error creating client: %v", err)
	}
	defer cc.Close()
	client := testpb.NewTestServiceClient(cc)

	// Confirm we are connected to the first server
	if res, err := client.UnaryCall(ctx, &testpb.SimpleRequest{}); err != nil || res.Username != one {
		t.Fatalf("UnaryCall(_) = %v, %v; want {Username: %q}, nil", res, err, one)
	}

	// Update resolver to report only the second server
	r.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: addr2}}})

	// Loop until new RPCs talk to server two.
	for i := 0; i < 2000; i++ {
		if res, err := client.UnaryCall(ctx, &testpb.SimpleRequest{}); err != nil {
			t.Fatalf("UnaryCall(_) = _, %v; want _, nil", err)
		} else if res.Username == two {
			break // pass
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEmptyAddrs verifies client behavior when a working connection is
// removed.  In pick first, it will continue using the old connection but in
// round robin it will not.
func (s) TestEmptyAddrs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Initialize server
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	s := grpc.NewServer()
	defer s.Stop()
	const one = "1"
	ts := &funcServer{
		unaryCall: func(ctx context.Context, in *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
			return &testpb.SimpleResponse{Username: one}, nil
		},
	}
	testpb.RegisterTestServiceServer(s, ts)
	go s.Serve(lis)

	// Initialize pickfirst client
	pfr, cleanup := manual.GenerateAndRegisterManualResolver()
	rn := make(chan struct{})
	pfr.ResolveNowCallback = func(resolver.ResolveNowOptions) {
		rn <- struct{}{}
	}
	defer cleanup()
	pfr.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: lis.Addr().String()}}})

	pfcc, err := grpc.DialContext(ctx, pfr.Scheme()+":///", grpc.WithInsecure())
	if err != nil {
		t.Fatalf("Error creating client: %v", err)
	}
	defer pfcc.Close()
	pfclient := testpb.NewTestServiceClient(pfcc)

	// Confirm we are connected to the server
	if res, err := pfclient.UnaryCall(ctx, &testpb.SimpleRequest{}); err != nil || res.Username != one {
		t.Fatalf("UnaryCall(_) = %v, %v; want {Username: %q}, nil", res, err, one)
	}

	// Remove all addresses.
	pfr.UpdateState(resolver.State{})
	// Wait for a ResolveNow call on the pick first client's resolver.
	select {
	case <-rn:
	case <-ctx.Done():
		t.Fatalf("ResolveNow() never invoked after providing empty addresses")
	}

	// Initialize roundrobin client
	rrr, cleanup := manual.GenerateAndRegisterManualResolver()
	defer cleanup()
	rrr.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: lis.Addr().String()}}})

	rrcc, err := grpc.DialContext(ctx, rrr.Scheme()+":///", grpc.WithInsecure(),
		grpc.WithDefaultServiceConfig(fmt.Sprintf(`{ "loadBalancingConfig": [{"%v": {}}] }`, roundrobin.Name)))
	if err != nil {
		t.Fatalf("Error creating client: %v", err)
	}
	defer rrcc.Close()
	rrclient := testpb.NewTestServiceClient(rrcc)

	// Confirm we are connected to the server
	if res, err := rrclient.UnaryCall(ctx, &testpb.SimpleRequest{}); err != nil || res.Username != one {
		t.Fatalf("UnaryCall(_) = %v, %v; want {Username: %q}, nil", res, err, one)
	}

	// Remove all addresses.
	rrr.UpdateState(resolver.State{})

	// Confirm RPCs eventually fail on round robin.
	for {
		if _, err := rrclient.UnaryCall(ctx, &testpb.SimpleRequest{}); err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatalf("round robin client did not fail after 5 seconds")
	}

	// Confirm several new RPCs succeed on pick first.  Because rr failed
	// before this, but we initialized and updated pf first, we should not need
	// to check this success case for as long.
	for i := 0; i < 10; i++ {
		if _, err := pfclient.UnaryCall(ctx, &testpb.SimpleRequest{}); err != nil {
			t.Fatalf("UnaryCall(_) = _, %v; want _, nil", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s) TestWaitForReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Initialize server
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Error while listening. Err: %v", err)
	}
	s := grpc.NewServer()
	defer s.Stop()
	const one = "1"
	ts := &funcServer{
		unaryCall: func(ctx context.Context, in *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
			return &testpb.SimpleResponse{Username: one}, nil
		},
	}
	testpb.RegisterTestServiceServer(s, ts)
	go s.Serve(lis)

	// Initialize client
	r, cleanup := manual.GenerateAndRegisterManualResolver()
	defer cleanup()

	cc, err := grpc.DialContext(ctx, r.Scheme()+":///", grpc.WithInsecure())
	if err != nil {
		t.Fatalf("Error creating client: %v", err)
	}
	defer cc.Close()
	client := testpb.NewTestServiceClient(cc)

	// Report an error so non-WFR RPCs will give up early.
	r.CC.ReportError(errors.New("fake resolver error"))

	// Ensure the client is not connected to anything and fails non-WFR RPCs.
	if res, err := client.UnaryCall(ctx, &testpb.SimpleRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("UnaryCall(_) = %v, %v; want _, Code()=%v", res, err, codes.Unavailable)
	}

	errChan := make(chan error, 1)
	go func() {
		if res, err := client.UnaryCall(ctx, &testpb.SimpleRequest{}, grpc.WaitForReady(true)); err != nil || res.Username != one {
			errChan <- fmt.Errorf("UnaryCall(_) = %v, %v; want {Username: %q}, nil", res, err, one)
		}
		close(errChan)
	}()

	select {
	case err := <-errChan:
		t.Errorf("unexpected receive from errChan before addresses provided")
		t.Fatal(err.Error())
	case <-time.After(5 * time.Millisecond):
	}

	// Resolve the server.  The WFR RPC should unblock and use it.
	r.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: lis.Addr().String()}}})

	if err := <-errChan; err != nil {
		t.Fatal(err.Error())
	}
}
