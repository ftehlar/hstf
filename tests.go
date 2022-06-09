package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"git.fd.io/govpp.git/api"
	"github.com/edwarnicke/govpp/binapi/af_packet"
	interfaces "github.com/edwarnicke/govpp/binapi/interface"
	"github.com/edwarnicke/govpp/binapi/interface_types"
	ip_types "github.com/edwarnicke/govpp/binapi/ip_types"
	"github.com/edwarnicke/govpp/binapi/session"
	"github.com/edwarnicke/govpp/binapi/tapv2"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
)

func startServerApp(running chan struct{}, done chan struct{}, env []string) {
	cmd := exec.Command("iperf3", "-4", "-s")
	if env != nil {
		cmd.Env = env
	}
	err := cmd.Start()
	if err != nil {
		log.Default().Errorf("failed to start server app: '%s'\n", err)
	}
	running <- struct{}{}
	<-done
	cmd.Process.Kill()
}

func startClientApp(env []string, finished chan struct{}) {
	defer func() {
		finished <- struct{}{}
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
				log.Default().Errorf("failed to start client app '%s'.\n%s", err, o)
				return
			}
			time.Sleep(1 * time.Second)
			nTries++
			continue
		}
		log.Default().Debugf("Client output: %s", o)
		break
	}
}

func TestLDPreloadIperfVpp() error {
	var tc TcContext
	var clnVclConf, srvVclConf, startup Stanza
	srvInstance := "vppsrv"
	clnInstance := "vppcln"
	srvRunDir := "/tmp/" + srvInstance
	clnRunDir := "/tmp/" + clnInstance
	srvVcl := fmt.Sprintf("/tmp/%s/vcl_srv.conf", srvInstance)
	clnVcl := fmt.Sprintf("/tmp/%s/vcl_cln.conf", clnInstance)

	ldpreload := os.Getenv("HSTF_LDPRELOAD")
	if ldpreload == "" {
		return fmt.Errorf("HSTF_LDPRELOAD not set")
	}
	ldpreload = "LD_PRELOAD=" + ldpreload

	tc.init(2)
	stopServerCh := make(chan struct{}, 1)
	serverRunning := make(chan struct{}, 1)
	tcFinished := make(chan struct{})

	startup.
		NewStanza("session").
		Append("enable").
		Append("use-app-socket-api").Close()

	ctx1, cancel1 := newVppContext()
	ctx2, cancel2 := newVppContext()
	defer cancel1()
	defer cancel2()

	log.Default().Debug("starting vpps")
	go startVpp(ctx1, &tc, cancel1, srvRunDir, startup.ToString(), configureLDPtest("vppsrv", "10.10.10.1/24", "1", 1))
	go startVpp(ctx2, &tc, cancel2, clnRunDir, startup.ToString(), configureLDPtest("vppcln", "10.10.10.2/24", "2", 2))

	// waiting for both vpps to finish configuration
	tc.wg.Wait()

	clnVclConf.
		NewStanza("vcl").
		Append("rx-fifo-size 4000000").
		Append("tx-fifo-size 4000000").
		Append("app-scope-local").
		Append("app-scope-global").
		Append("use-mq-eventfd").
		Append(fmt.Sprintf("app-socket-api /tmp/%s/var/run/vpp/app_ns_sockets/2", clnInstance)).Close().
		SaveToFile(clnVcl)

	srvVclConf.
		NewStanza("vcl").
		Append("rx-fifo-size 4000000").
		Append("tx-fifo-size 4000000").
		Append("app-scope-local").
		Append("app-scope-global").
		Append("use-mq-eventfd").
		Append(fmt.Sprintf("app-socket-api /tmp/%s/var/run/vpp/app_ns_sockets/1", srvInstance)).Close().
		SaveToFile(srvVcl)
	log.Default().Debug("attaching clients")

	srvEnv := append(os.Environ(), ldpreload, "VCL_CONFIG="+srvVcl)
	go startServerApp(serverRunning, stopServerCh, srvEnv)

	<-serverRunning

	clnEnv := append(os.Environ(), ldpreload, "VCL_CONFIG="+clnVcl)
	go startClientApp(clnEnv, tcFinished)

	// wait for client
	<-tcFinished

	// stop server
	stopServerCh <- struct{}{}
	return nil
}

func configureHttpTps(runDir, server_ip, port string) ConfFn {
	return func(ctx context.Context,
		vppConn api.Connection) error {
		client_ip4 := "172.0.0.2"
		_, err := session.NewServiceClient(vppConn).SessionEnableDisable(ctx, &session.SessionEnableDisable{
			IsEnable: true,
		})
		if err != nil {
			return err
		}
		Vppcli(runDir, "create tap id 0 host-ip4-addr "+client_ip4+"/24")
		Vppcli(runDir, "set int ip addr tap0 "+server_ip+"/24")
		Vppcli(runDir, "set int state tap0 up")
		Vppcli(runDir, "http tps uri tcp://0.0.0.0/"+port)
		return nil
	}
}

func configureLDPtest(ifName, interfaceAddress, namespaceId string, secret uint64) ConfFn {
	return func(ctx context.Context,
		vppConn api.Connection) error {

		swIfIndex, err := configureAfPacket(ctx, vppConn, ifName, interfaceAddress)
		if err != nil {
			log.FromContext(ctx).Fatalf("failed to create af packet: %v", err)
		}
		_, er := session.NewServiceClient(vppConn).AppNamespaceAddDelV2(ctx, &session.AppNamespaceAddDelV2{
			Secret:      secret,
			SwIfIndex:   swIfIndex,
			NamespaceID: namespaceId,
		})
		if er != nil {
			log.FromContext(ctx).Fatal("add app namespace ", err)
			return err
		}

		_, er1 := session.NewServiceClient(vppConn).SessionEnableDisable(ctx, &session.SessionEnableDisable{
			IsEnable: true,
		})
		if er1 != nil {
			log.FromContext(ctx).Fatalf("session enable %w", err)
			return err
		}
		return nil
	}
}

func TestIperfLinux() error {
	tcFinished := make(chan struct{})
	stopServerCh := make(chan struct{})
	serverRunning := make(chan struct{}, 1)

	go startServerApp(serverRunning, stopServerCh, nil)
	<-serverRunning
	go startClientApp(nil, tcFinished)

	<-tcFinished
	stopServerCh <- struct{}{}

	return nil
}

func startWget(finished chan error, server_ip, port string) {
	fname := "test_file_10M"
	defer func() {
		finished <- errors.New("wget error")
	}()

	cmd := exec.Command("wget", "-O", "/dev/null", server_ip+":"+port+"/"+fname)
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Errorf("wget error: '%s'.\n%s", err, o)
		return
	}
	log.Default().Debugf("Client output: %s", o)
	finished <- nil
}

// unused
func _configureVppTap(ctx context.Context, con api.Connection, name, addr1, addr2 string) error {
	ifaceClient := interfaces.NewServiceClient(con)
	var pref ip_types.IP4Prefix
	pref.UnmarshalText([]byte(addr1))
	rsp, err := tapv2.NewServiceClient(con).TapCreateV2(ctx,
		&tapv2.TapCreateV2{HostIP4PrefixSet: true,
			HostIP4Prefix: ip_types.IP4AddressWithPrefix(pref),
		})
	if err != nil {
		return fmt.Errorf("failed to configure tap: %v", err)
	}

	_, err = ifaceClient.SwInterfaceSetFlags(ctx, &interfaces.SwInterfaceSetFlags{
		SwIfIndex: rsp.SwIfIndex,
		Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
	})
	if err != nil {
		log.FromContext(ctx).Fatal("set interface state up failed: ", err)
		return err
	}

	ipPrefix, err := ip_types.ParseAddressWithPrefix(addr2)
	if err != nil {
		log.FromContext(ctx).Fatal("parse ip address ", err)
		return err
	}
	ipAddress := &interfaces.SwInterfaceAddDelAddress{
		IsAdd:     true,
		SwIfIndex: rsp.SwIfIndex,
		Prefix:    ipPrefix,
	}
	_, errx := ifaceClient.SwInterfaceAddDelAddress(ctx, ipAddress)
	if errx != nil {
		log.FromContext(ctx).Fatal("add ip address ", err)
		return err
	}
	return nil
}

func configureAfPacket(ctx context.Context, vppCon api.Connection,
	name, interfaceAddress string) (interface_types.InterfaceIndex, error) {
	ifaceClient := interfaces.NewServiceClient(vppCon)
	afPacketCreate := &af_packet.AfPacketCreateV2{
		UseRandomHwAddr: true,
		HostIfName:      name,
		NumRxQueues:     1,
	}
	afPacketCreateRsp, err := af_packet.NewServiceClient(vppCon).AfPacketCreateV2(ctx, afPacketCreate)
	if err != nil {
		log.FromContext(ctx).Fatalf("failed to create af packet: %v", err)
		return 0, err
	}
	_, err = ifaceClient.SwInterfaceSetFlags(ctx, &interfaces.SwInterfaceSetFlags{
		SwIfIndex: afPacketCreateRsp.SwIfIndex,
		Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
	})
	if err != nil {
		log.FromContext(ctx).Fatal("set interface state up failed: ", err)
		return 0, err
	}
	ipPrefix, err := ip_types.ParseAddressWithPrefix(interfaceAddress)
	if err != nil {
		log.FromContext(ctx).Fatal("parse ip address ", err)
		return 0, err
	}
	ipAddress := &interfaces.SwInterfaceAddDelAddress{
		IsAdd:     true,
		SwIfIndex: afPacketCreateRsp.SwIfIndex,
		Prefix:    ipPrefix,
	}
	_, errx := ifaceClient.SwInterfaceAddDelAddress(ctx, ipAddress)
	if errx != nil {
		log.FromContext(ctx).Fatal("add ip address ", err)
		return 0, err
	}
	return afPacketCreateRsp.SwIfIndex, nil
}

func configureProxyTcp(ifName0, ipAddr0, ifName1, ipAddr1 string) ConfFn {
	return func(ctx context.Context,
		vppConn api.Connection) error {

		_, err := configureAfPacket(ctx, vppConn, ifName0, ipAddr0)
		if err != nil {
			log.FromContext(ctx).Fatalf("failed to create af packet: %v", err)
			return err
		}
		_, err = configureAfPacket(ctx, vppConn, ifName1, ipAddr1)
		if err != nil {
			log.FromContext(ctx).Fatalf("failed to create af packet: %v", err)
			return err
		}
		return nil
	}
}

func startHttpServer(running chan struct{}, done chan struct{}, addressPort, netNs string) {
	cmd := NewCommand([]string{"./tools/http_server/http_server", addressPort}, netNs)
	err := cmd.Start()
	if err != nil {
		fmt.Println("Failed to start http server")
		return
	}
	running <- struct{}{}
	<-done
	cmd.Process.Kill()
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

func TestVppProxyHttpTcp() error {
	vppConf := configureProxyTcp("vpp0", "10.0.0.2/24", "vpp1", "10.0.1.2/24")
	runDir := "/tmp/"
	instance := "vpp-proxy"
	return testProxyHttpTcp(instance, &Stanza{}, vppConf, func() error {
		// configure test proxy on vpp
		Vppcli(runDir+instance, "test proxy server server-uri tcp://10.0.0.2/555 client-uri tcp://10.0.1.1/666")
		return nil
	})
}

func TestEnvoyProxyHttpTcp() error {
	var startup Stanza
	startup.
		NewStanza("session").
		Append("enable").
		Append("use-app-socket-api").
		Append("evt_qs_memfd_seg").
		Append("event-queue-length 100000").Close()

	defer func() {
		RunCommand([]string{"docker", "stop", "envoy"}, "")
	}()

	instance := "vpp-envoy"
	return testProxyHttpTcp(instance, &startup,
		configureProxyTcp("vpp0", "10.0.0.2/24", "vpp1", "10.0.1.2/24"),
		func() error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			_, err0 := RunCommand([]string{"chmod", "777", "-R",
				fmt.Sprintf("/tmp/%s", instance)}, "")
			if err0 != nil {
				return fmt.Errorf("failed to chmod socket file: %v", err0)
			}

			c := []string{"docker", "run", "--rm", "--name", "envoy",
				"-v", fmt.Sprintf("%s/envoy/proxy.yaml:/etc/envoy/envoy.yaml", wd),
				"-v", fmt.Sprintf("/tmp/%s/var/run/vpp:/var/run/vpp", instance),
				"-v", fmt.Sprintf("%s/envoy:/tmp", wd),
				"-e", "VCL_CONFIG=/tmp/vcl.conf",
				"envoyproxy/envoy-contrib:v1.21-latest"}
			fmt.Println(c)
			cmd := NewCommand(c, "")
			err = cmd.Start()
			if err != nil {
				return err
			}
			return nil
		})
}

func testProxyHttpTcp(instance string, stanza *Stanza, vppConf ConfFn, proxySetup func() error) error {
	const outputFile = "test.data"
	const srcFile = "10M"
	var tc TcContext
	stopServer := make(chan struct{}, 1)
	serverRunning := make(chan struct{}, 1)
	tc.init(1)

	runDir := "/tmp/" + instance
	ctx, cancel := newVppContext()
	defer cancel()
	go startVpp(ctx, &tc, cancel, runDir, fmt.Sprintf(configTemplate, runDir, ""), vppConf)

	tc.wg.Wait()
	fmt.Println("VPP running and configured...")

	if err := proxySetup(); err != nil {
		return fmt.Errorf("failed to setup proxy: %v", err)
	}

	// create test file
	c := []string{"truncate", "-s", srcFile, srcFile}
	_, err := RunCommand(c, "server")
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

	fmt.Println("https server started...")

	c = []string{"wget", "--retry-connrefused", "--retry-on-http-error=503", "--tries=10", "10.0.0.2:555/" + srcFile, "-O", outputFile}
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

const configTemplate = `unix {
  nodaemon
  log %[1]s/var/log/vpp/vpp.log
  full-coredump
  cli-listen %[1]s/var/run/vpp/cli.sock
  runtime-dir %[1]s/var/run
  gid vpp
}

api-trace {
  on
}

api-segment {
  gid vpp
}

socksvr {
  socket-name %[1]s/var/run/vpp/api.sock
}

statseg {
  socket-name %[1]s/var/run/vpp/stats.sock
}

%[2]s
`

func TestHttpTps() error {
	finished := make(chan error, 1)
	var rc error
	var tc TcContext
	tc.init(1)
	server_ip := "172.0.0.1"
	port := "8080"
	runDir := "/tmp/vpp-tps"

	ctx, cancel := newVppContext()
	defer cancel()

	log.Default().Debug("starting vpp..")
	go startVpp(ctx, &tc, cancel, runDir, fmt.Sprintf(configTemplate, runDir, ""),
		configureHttpTps(runDir, server_ip, port))

	tc.wg.Wait()

	go startWget(finished, server_ip, port)
	// wait for client
	err := <-finished
	if err != nil {
		rc = err
	}
	return rc
}
