package main

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/stretchr/testify/suite"
)

type NoTopoSuite struct {
	suite.Suite
}

func (s *NoTopoSuite) SetupSuite()    {}
func (s *NoTopoSuite) TearDownSuite() {}

type TapSuite struct {
	suite.Suite
	teardownSuite func()
}

func (s *TapSuite) SetupSuite() {
	time.Sleep(1 * time.Second)
	s.teardownSuite = setupSuite(&s.Suite, "tap")
}

func (s *TapSuite) TearDownSuite() {
	s.teardownSuite()
}

type Veths2Suite struct {
	suite.Suite
	teardownSuite func()
}

func (s *Veths2Suite) SetupSuite() {
	time.Sleep(1 * time.Second)
	s.teardownSuite = setupSuite(&s.Suite, "2peerVeth")
}

func (s *Veths2Suite) TearDownSuite() {
	s.teardownSuite()
}

type NsSuite struct {
	suite.Suite
	teardownSuite func()
}

func (s *NsSuite) SetupSuite() {
	s.teardownSuite = setupSuite(&s.Suite, "ns")
}

func (s *NsSuite) TearDownSuite() {
	s.teardownSuite()
}

func (s *Veths2Suite) TestLDPreloadIperfVpp() {
	t := s.T()
	var clnVclConf, srvVclConf Stanza
	// RunCommand([]string{"docker", "volume", "create", "--name=envoy-vol"}, "")

	srvInstance := "vpp-ldp-srv"
	clnInstance := "vpp-ldp-cln"
	srvPath := "/tmp/" + srvInstance
	clnPath := "/tmp/" + clnInstance
	srvVcl := srvPath + "/vcl_srv.conf"
	clnVcl := clnPath + "/vcl_cln.conf"

	RunCommand([]string{"mkdir", srvPath}, "")
	RunCommand([]string{"mkdir", clnPath}, "")

	ldpreload := os.Getenv("HSTF_LDPRELOAD")
	s.Assert().NotEqual("", ldpreload)

	ldpreload = "LD_PRELOAD=" + ldpreload

	stopServerCh := make(chan struct{}, 1)
	srvCh := make(chan error, 1)
	clnCh := make(chan error)

	log.Default().Debug("starting vpps")
	o, err := RunCommand([]string{"docker", "run", "-d", "--privileged",
		"-v", fmt.Sprintf("/tmp/%s:/tmp", srvInstance),
		"--network", "host", "--rm", "--name", srvInstance, "hstf/vpp"}, "")
	fmt.Println(string(o))
	s.Assert().Nil(err)
	defer func() { RunCommand([]string{"docker", "stop", srvInstance}, "") }()

	o, err = RunCommand([]string{"docker", "run", "-d", "--privileged",
		"-v", fmt.Sprintf("/tmp/%s:/tmp", clnInstance),
		"--network", "host", "--rm", "--name", clnInstance, "hstf/vpp"}, "")
	fmt.Println(string(o))
	s.Assert().Nil(err)
	defer func() { RunCommand([]string{"docker", "stop", clnInstance}, "") }()

	o, err = dockerExec([]string{"/hstf", "ld-preload", "srv"}, srvInstance)
	fmt.Println(string(o))
	s.Assert().Nil(err)

	o, err = dockerExec([]string{"/hstf", "ld-preload", "cln"}, clnInstance)
	fmt.Println(string(o))
	s.Assert().Nil(err)

	err = clnVclConf.
		NewStanza("vcl").
		Append("rx-fifo-size 4000000").
		Append("tx-fifo-size 4000000").
		Append("app-scope-local").
		Append("app-scope-global").
		Append("use-mq-eventfd").
		Append(fmt.Sprintf("app-socket-api /tmp/%s/var/run/app_ns_sockets/2", clnInstance)).Close().
		SaveToFile(clnVcl)
	if err != nil {
		t.Errorf("%v", err)
		t.FailNow()
	}

	err = srvVclConf.
		NewStanza("vcl").
		Append("rx-fifo-size 4000000").
		Append("tx-fifo-size 4000000").
		Append("app-scope-local").
		Append("app-scope-global").
		Append("use-mq-eventfd").
		Append(fmt.Sprintf("app-socket-api /tmp/%s/var/run/app_ns_sockets/1", srvInstance)).Close().
		SaveToFile(srvVcl)
	if err != nil {
		t.Errorf("%v", err)
		t.FailNow()
	}
	log.Default().Debug("attaching server to vpp")

	// FIXME
	time.Sleep(5 * time.Second)

	srvEnv := append(os.Environ(), ldpreload, "VCL_CONFIG="+srvVcl)
	go StartServerApp(srvCh, stopServerCh, srvEnv)

	err = <-srvCh
	if err != nil {
		s.FailNow("vcl server", "%v", err)
	}

	log.Default().Debug("attaching client to vpp")
	clnEnv := append(os.Environ(), ldpreload, "VCL_CONFIG="+clnVcl)
	go StartClientApp(clnEnv, clnCh)

	// wait for client's result
	err = <-clnCh
	if err != nil {
		s.Failf("client", "%v", err)
	}

	// stop server
	stopServerCh <- struct{}{}
}

func dockerExec(cmd []string, instance string) ([]byte, error) {
	c := append([]string{"docker", "exec", "-d", instance}, cmd...)
	fmt.Println(c)
	return RunCommand(c, "")
}

func testProxyHttpTcp(dockerInstance string, proxySetup func() error) error {
	const outputFile = "test.data"
	const srcFile = "10M"
	stopServer := make(chan struct{}, 1)
	serverRunning := make(chan struct{}, 1)

	// run container
	o, err := RunCommand([]string{"docker", "run", "--cap-add=all", "-d",
		"--privileged", "--network", "host", "--rm", "--name", dockerInstance,
		"-v", fmt.Sprintf("envoy-vol:/tmp/%s", dockerInstance),
		"hstf/vpp"}, "")
	fmt.Println(string(o))
	if err != nil {
		return fmt.Errorf("failed to start container: %v", err)
	} else {
		fmt.Println("container running..")
	}
	defer func() { RunCommand([]string{"docker", "stop", dockerInstance}, "") }()

	// start & configure vpp in the container
	o, err = dockerExec([]string{"/hstf", dockerInstance}, dockerInstance)
	fmt.Println(string(o))
	if err != nil {
		return fmt.Errorf("error starting vpp in container: %v", err)
	}

	fmt.Println("VPP running and configured...")

	if err := proxySetup(); err != nil {
		return fmt.Errorf("failed to setup proxy: %v", err)
	}

	// create test file
	c := []string{"truncate", "-s", srcFile, srcFile}
	_, err = RunCommand(c, "server")
	if err != nil {
		return fmt.Errorf("failed to run truncate command")
	}
	defer func() {
		os.Remove(srcFile)
	}()

	go startHttpServer(serverRunning, stopServer, ":666", "server")
	// TODO better error handling and recovery
	<-serverRunning

	defer func(chan struct{}) {
		stopServer <- struct{}{}
	}(stopServer)

	fmt.Println("http server started...")

	c = []string{"wget", "--retry-connrefused", "--retry-on-http-error=503",
		"--tries=10", "10.0.0.2:555/" + srcFile, "-O", outputFile}
	o, err1 := RunCommand(c, "client")
	if err1 != nil {
		return fmt.Errorf("failed to run wget: %v %v", err1, string(o))
	}
	stopServer <- struct{}{}

	defer func() {
		os.Remove(outputFile)
	}()

	if err = assertFileSize(outputFile, srcFile); err != nil {
		return err
	}
	return nil
}

func configureVppProxy() error {
	// configure test proxy on vpp
	for i := 0; i < 5; i++ {
		dockerExec([]string{"vppctl", "test", "proxy", "server", "server-uri",
			"tcp://10.0.0.2/555", "client-uri", "tcp://10.0.1.1/666"}, "vpp-proxy")
		// FIXME we don't know when vpp is ready
		time.Sleep(1 * time.Second)
	}
	return nil
}

func (s *NsSuite) TestVppProxyHttpTcp() {
	dockerInstance := "vpp-proxy"
	err := testProxyHttpTcp(dockerInstance, configureVppProxy)
	s.Assert().Nil(err)
}

func startEnvoy(t *testing.T, dockerInstance string) error {
	errCh := make(chan error)
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	c := []string{"docker", "run", "--rm", "--name", "envoy",
		"-v", fmt.Sprintf("%s/envoy/proxy.yaml:/etc/envoy/envoy.yaml", wd),
		"-v", fmt.Sprintf("envoy-vol:/tmp/%s", dockerInstance),
		"-v", fmt.Sprintf("%s/envoy:/tmp", wd),
		"-e", "VCL_CONFIG=/tmp/vcl.conf",
		"envoyproxy/envoy-contrib:v1.21-latest"}
	fmt.Println(c)

	go func(errCh chan error) {
		count := 0
		for ; count < 5; count++ {
			cmd := NewCommand(c, "")
			err = cmd.Start()
			if err != nil {
				continue
			}
			err = cmd.Wait()
			if err != nil {
				fmt.Println(err)
				continue
			}
		}
		errCh <- fmt.Errorf("failed to start docker after %d attempts: %v", count, err)
	}(errCh)

	// if there is already an error in the channel, capture it
	select {
	case err := <-errCh:
		fmt.Printf("error while starting envoy: %v", err)
		return err
	default:
	}

	go func(errCh <-chan error) {
		err := <-errCh
		fmt.Printf("error while starting envoy: %v", err)
	}(errCh)
	return nil
}

func (s *NsSuite) TestEnvoyProxyHttpTcp() {
	RunCommand([]string{"docker", "volume", "create", "--name=envoy-vol"}, "")
	defer func() {
		RunCommand([]string{"docker", "stop", "envoy"}, "")
	}()

	dockerInstance := "vpp-envoy"
	err := testProxyHttpTcp(dockerInstance, func() error {
		return startEnvoy(s.T(), dockerInstance)
	})
	s.Assert().Nil(err)
}

func setupSuite(s *suite.Suite, topo string) func() {
	t := s.T()
	var topoBase TopoBase
	err := topoBase.LoadTopologies("topo/")
	if err != nil {
		t.Fatalf("error on loading topology definitions: %v", err)
	}
	topoDesc := topoBase.FindTopoByName(topo)
	if topoDesc == nil {
		t.Fatalf("topo definition for '%s' not found", topo)
	}
	err = topoDesc.Configure()
	if err != nil {
		t.Fatalf("failed to configure %s: %v", topo, err)
	}

	t.Logf("topo %s loaded", topo)
	return func() {
		topoDesc.RemoveConfig()
	}
}

func (s *NsSuite) TestHttpTps() {
	t := s.T()
	finished := make(chan error, 1)
	server_ip := "10.0.0.2"
	port := "8080"
	dockerInstance := "http-tps"

	t.Log("starting vpp..")

	o, err := RunCommand([]string{"docker", "run", "--cap-add=all", "-d",
		"--privileged", "--network", "host", "--rm", "--name", dockerInstance,
		"hstf/vpp"}, "")
	fmt.Println(string(o))
	s.Assert().Nil(err)
	defer func() { RunCommand([]string{"docker", "stop", dockerInstance}, "") }()

	// start & configure vpp in the container
	o, err = dockerExec([]string{"/hstf", dockerInstance}, dockerInstance)
	fmt.Println(string(o))
	s.Assert().Nil(err)

	go startWget(finished, server_ip, port, "client")
	// wait for client
	err = <-finished
	s.Assert().Nil(err)
}

func assertFileSize(f1, f2 string) error {
	fi1, err := os.Stat(f1)
	if err != nil {
		return err
	}

	fi2, err1 := os.Stat(f2)
	if err1 != nil {
		return err1
	}

	if fi1.Size() != fi2.Size() {
		return fmt.Errorf("file sizes differ (%d vs %d)", fi1.Size(), fi2.Size())
	}
	return nil
}

func StartServerApp(running chan error, done chan struct{}, env []string) {
	cmd := exec.Command("iperf3", "-4", "-s")
	if env != nil {
		cmd.Env = env
	}
	err := cmd.Start()
	if err != nil {
		msg := fmt.Errorf("failed to start iperf server: %v", err)
		log.Default().Error(msg)
		running <- msg
		return
	}
	running <- nil
	<-done
	cmd.Process.Kill()
}

func StartClientApp(env []string, clnCh chan error) {
	defer func() {
		clnCh <- nil
	}()

	nTries := 0

	for {
		cmd := exec.Command("iperf3", "-c", "10.10.10.1", "-u", "-l", "1460", "-b", "10g")
		if env != nil {
			cmd.Env = env
		}
		o, err := cmd.CombinedOutput()
		if err != nil {
			if nTries > 5 {
				clnCh <- fmt.Errorf("failed to start client app '%s'.\n%s", err, o)
				return
			}
			time.Sleep(1 * time.Second)
			nTries++
			continue
		} else {
			log.Default().Debugf("Client output: %s", o)
		}
		break
	}
}
func (s *TapSuite) TestLinuxIperf() {
	t := s.T()
	clnCh := make(chan error)
	stopServerCh := make(chan struct{})
	srvCh := make(chan error, 1)
	defer func() {
		stopServerCh <- struct{}{}
	}()

	go StartServerApp(srvCh, stopServerCh, nil)
	err := <-srvCh
	if err != nil {
		t.Errorf("%v", err)
		t.FailNow()
	}
	t.Log("server running")
	go StartClientApp(nil, clnCh)
	t.Log("client running")
	err = <-clnCh
	if err != nil {
		s.Failf("client", "%v", err)
	}
	t.Log("Test completed")
}

func TestNoTopo(t *testing.T) {
	var m NoTopoSuite
	suite.Run(t, &m)
}

func TestTapSuite(t *testing.T) {
	var m TapSuite
	suite.Run(t, &m)
}

func TestNs(t *testing.T) {
	var m NsSuite
	suite.Run(t, &m)
}

func TestVeths2(t *testing.T) {
	var m Veths2Suite
	suite.Run(t, &m)

}

func TestDocker(t *testing.T) {
	t.Log("VPP in docker test")
}
