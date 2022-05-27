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

	if config.LdPreload == "" {
		return fmt.Errorf("LD_COFNIG not set")
	}
	ldPreload := "LD_PRELOAD=" + config.LdPreload

	tc.init(2)
	stopServerCh := make(chan struct{}, 1)
	serverRunning := make(chan struct{}, 1)

	tcFinished := make(chan struct{})

	var startup Stanza
	startup.
		NewStanza("session").
		Append("enable").
		Append("use-app-socket-api").Close()

	log.Default().Debug("starting vpps")
	go startVpp(&tc, "vppsrv", &startup, configureLDPtest("vppsrv", "10.10.10.1/24", "1", 1))
	go startVpp(&tc, "vppcln", &startup, configureLDPtest("vppcln", "10.10.10.2/24", "2", 2))

	cancelFns := receiveCancelFns(&tc, 2)

	// waiting for both vpps to finish configuration
	tc.wg.Wait()

	log.Default().Debug("attaching clients")

	srvEnv := append(os.Environ(), ldPreload, "VCL_CONFIG=vcl_srv.conf")
	go startServerApp(serverRunning, stopServerCh, srvEnv)

	<-serverRunning

	clnEnv := append(os.Environ(), ldPreload, "VCL_CONFIG=vcl_cln.conf")
	go startClientApp(clnEnv, tcFinished)

	// wait for client
	<-tcFinished

	// stop server
	stopServerCh <- struct{}{}

	// stop vpp routines
	for _, fn := range cancelFns {
		fn()
	}
	return nil
}

func configureHttpTps(server_ip, port string) ConfFn {
	return func(ctx context.Context,
		vppConn api.Connection) error {
		client_ip4 := "172.0.0.2"
		_, err := session.NewServiceClient(vppConn).SessionEnableDisable(ctx, &session.SessionEnableDisable{
			IsEnable: true,
		})
		if err != nil {
			return err
		}
		Vppcli("vpp-tps", "create tap id 0 host-ip4-addr "+client_ip4+"/24")
		Vppcli("vpp-tps", "set int ip addr tap0 "+server_ip+"/24")
		Vppcli("vpp-tps", "set int state tap0 up")
		Vppcli("vpp-tps", "http tps uri tcp://0.0.0.0/"+port)
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

// gather all cancel functions
func receiveCancelFns(tc *TcContext, n int) []context.CancelFunc {
	var res []context.CancelFunc
	for i := 0; i < n; i++ {
		res = append(res, <-tc.mainCh)
	}
	return res
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
	cmd := StartCommand([]string{"./tools/http_server/http_server", addressPort}, netNs)
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

func TestProxyTcp() error {
	const outputFile = "test.data"
	const srcFile = "10M"
	var tc TcContext
	stopServer := make(chan struct{}, 1)
	serverRunning := make(chan struct{}, 1)
	tc.init(1)

	go startVpp(&tc, "vpp-proxytcp", &Stanza{},
		configureProxyTcp("vpp0", "10.0.0.2/24", "vpp1", "10.0.1.2/24"))

	cancelFns := receiveCancelFns(&tc, 1)
	for _, fn := range cancelFns {
		defer fn()
	}
	tc.wg.Wait()

	// configure proxy
	Vppcli("vpp-proxytcp", "test proxy server server-uri tcp://10.0.0.2/555 client-uri tcp://10.0.1.1/666")

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

	c = []string{"wget", "10.0.0.2:555/" + srcFile, "-O", outputFile}
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

func TestHttpTps() error {
	finished := make(chan error, 1)
	var rc error
	var tc TcContext
	tc.init(1)
	server_ip := "172.0.0.1"
	port := "8080"

	log.Default().Debug("starting vpp..")
	go startVpp(&tc, "vpp-tps", &Stanza{}, configureHttpTps(server_ip, port))
	cancelFns := receiveCancelFns(&tc, 1)

	tc.wg.Wait()

	go startWget(finished, server_ip, port)
	// wait for client
	err := <-finished
	if err != nil {
		rc = err
	}

	for _, fn := range cancelFns {
		fn()
	}
	return rc
}
