package core

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/anz-bank/sysl-go/config"
	test "github.com/anz-bank/sysl-go/core/testdata/proto"
	"github.com/anz-bank/sysl-go/handlerinitialiser"
	"github.com/anz-bank/sysl-go/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const testPort = 8888

type TestServer struct{}

func (*TestServer) Test(ctx context.Context, req *test.TestRequest) (*test.TestReply, error) {
	return &test.TestReply{Field1: req.GetField1()}, nil
}

func localServer() config.GRPCServerConfig {
	return config.GRPCServerConfig{
		CommonServerConfig: config.CommonServerConfig{
			HostName: "localhost",
			Port:     testPort,
		},
	}
}

func localSecureServer() config.GRPCServerConfig {
	minVer := "1.2"
	maxVer := "1.3"
	certPath := "testdata/creds/server1.pem"
	keyPath := "testdata/creds/server1.key"
	clientAuth := "NoClientCert"
	ciphers := []string{"TLS_RSA_WITH_AES_256_CBC_SHA"}
	return config.GRPCServerConfig{
		CommonServerConfig: config.CommonServerConfig{
			HostName: "localhost",
			Port:     testPort,
			TLS: &config.TLSConfig{
				MinVersion: &minVer,
				MaxVersion: &maxVer,
				ClientAuth: &clientAuth,
				Ciphers:    ciphers,
				ServerIdentities: []*config.ServerIdentityConfig{
					{
						CertKeyPair: &config.CertKeyPair{
							CertPath: &certPath,
							KeyPath:  &keyPath,
						},
					},
				},
			},
		}}
}

type ServerReg struct {
	svr           TestServer
	methodsCalled map[string]bool
}

func (r *ServerReg) RegisterServer(ctx context.Context, server *grpc.Server) {
	r.methodsCalled["RegisterServer"] = true
	test.RegisterTestServiceServer(server, &r.svr)
}

type GrpcHandler struct {
	cfg           config.GRPCServerConfig
	reg           ServerReg
	methodsCalled map[string]bool
}

func (h *GrpcHandler) Interceptors() []grpc.UnaryServerInterceptor {
	h.methodsCalled["Interceptors"] = true

	return []grpc.UnaryServerInterceptor{}
}

func (h *GrpcHandler) EnabledGrpcHandlers() []handlerinitialiser.GrpcHandlerInitialiser {
	h.methodsCalled["EnabledGrpcHandlers"] = true

	return []handlerinitialiser.GrpcHandlerInitialiser{&h.reg}
}

func (h *GrpcHandler) GrpcAdminServerConfig() *config.CommonServerConfig {
	h.methodsCalled["GrpcAdminServerConfig"] = true
	return &h.cfg.CommonServerConfig
}

func (h *GrpcHandler) GrpcPublicServerConfig() *config.GRPCServerConfig {
	h.methodsCalled["GrpcPublicServerConfig"] = true
	return &h.cfg
}

func connectAndCheckReturn(ctx context.Context, t *testing.T, securityOption grpc.DialOption) {
	conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", testPort), securityOption, grpc.WithBlock())
	require.NoError(t, err)
	defer conn.Close()
	client := test.NewTestServiceClient(conn)
	resp, err := client.Test(ctx, &test.TestRequest{Field1: "test"})
	require.NoError(t, err)
	require.Equal(t, "test", resp.GetField1())
}

func Test_makeGrpcListenFuncListens(t *testing.T) {
	ctx, _ := testutil.NewTestContextWithLogger()

	s := grpc.NewServer()
	defer s.GracefulStop()
	test.RegisterTestServiceServer(s, &TestServer{})

	srv := grpcServer{ctx: ctx, cfg: localServer(), server: s}
	go func() {
		err := srv.Start()
		require.NoError(t, err)
	}()

	connectAndCheckReturn(ctx, t, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func Test_encryptionConfigUsed(t *testing.T) {
	t.Skip("Skipping as required certs not present")
	ctx, logger := testutil.NewTestContextWithLogger()

	cfg := localSecureServer()

	s := grpc.NewServer()
	defer s.GracefulStop()
	test.RegisterTestServiceServer(s, &TestServer{})

	srv := grpcServer{ctx: ctx, cfg: cfg, server: s}
	go func() {
		err := srv.Start()
		require.NoError(t, err)
	}()

	creds, err := credentials.NewClientTLSFromFile("testdata/creds/ca.pem", "x.test.youtube.com")
	require.NoError(t, err)

	connectAndCheckReturn(ctx, t, grpc.WithTransportCredentials(creds))
	for _, entry := range logger.Entries() {
		t.Log(entry.Message)
	}
}

func Test_serverUsesGivenLogger(t *testing.T) {
	os.Setenv("GRPC_GO_LOG_VERBOSITY_LEVEL", "99")

	ctx, logger := testutil.NewTestContextWithLogger()

	s := grpc.NewServer()
	defer s.GracefulStop()
	test.RegisterTestServiceServer(s, &TestServer{})

	setLogger(ctx)

	srv := prepareGrpcServerListener(ctx, s, localServer(), "")
	go func() {
		err := srv.Start()
		require.NoError(t, err)
	}()

	conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", testPort), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)
	defer conn.Close()

	var connecting bool
	cre := regexp.MustCompile(`ClientConn switching balancer`)
	for _, entry := range logger.Entries() {
		if connecting {
			break
		}
		connecting = cre.MatchString(entry.Message)
	}
	require.True(t, connecting)
}

func Test_libMakesCorrectHandlerCalls(t *testing.T) {
	ctx, _ := testutil.NewTestContextWithLogger()

	manager := &GrpcHandler{
		cfg: localServer(),
		reg: ServerReg{
			svr:           TestServer{},
			methodsCalled: make(map[string]bool),
		},
		methodsCalled: make(map[string]bool),
	}

	// Adapt deprecated GrpcManager type as GrpcServerManager struct
	grpcServerManager, err := newGrpcServerManagerFromGrpcManager(ctx, manager)
	require.NoError(t, err)

	srv := configurePublicGrpcServerListener(ctx, *grpcServerManager, nil)
	require.NotNil(t, srv)

	defer func() {
		_ = srv.Stop()
	}()

	go func() {
		err := srv.Start()
		require.NoError(t, err)
	}()

	connectAndCheckReturn(ctx, t, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.True(t, manager.methodsCalled["Interceptors"])
	require.True(t, manager.methodsCalled["EnabledGrpcHandlers"])
	require.True(t, manager.methodsCalled["GrpcPublicServerConfig"])
	require.True(t, manager.reg.methodsCalled["RegisterServer"])
}
