package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"git.fd.io/govpp.git/api"
	"github.com/edwarnicke/vpphelper"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"

	// "github.com/edwarnicke/exechelper"
	"github.com/edwarnicke/govpp/binapi/af_packet"
	interfaces "github.com/edwarnicke/govpp/binapi/interface"
	"github.com/edwarnicke/govpp/binapi/interface_types"
	ip_types "github.com/edwarnicke/govpp/binapi/ip_types"
	"github.com/edwarnicke/govpp/binapi/session"
)

type TcContext struct {
	wg     *sync.WaitGroup
	mainCh chan context.CancelFunc
}

type VppConfig struct {
	ifName           string
	interfaceAddress string
	namespaceId      string
	secret           uint64
}

func exitOnErrCh(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		log.FromContext(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}

func configureLDPtest(ifName, interfaceAddress, namespaceId string, secret uint64) ConfFn {
	return func(ctx context.Context,
		vppConn api.Connection) error {
		ifaceClient := interfaces.NewServiceClient(vppConn)
		afPacketCreate := &af_packet.AfPacketCreateV2{
			UseRandomHwAddr: true,
			HostIfName:      ifName,
			NumRxQueues:     1,
		}
		afPacketCreateRsp, err := af_packet.NewServiceClient(vppConn).AfPacketCreateV2(ctx, afPacketCreate)
		if err != nil {
			log.FromContext(ctx).Fatal("failed to create af packet: ", err)
			return err
		}
		_, err = ifaceClient.SwInterfaceSetFlags(ctx, &interfaces.SwInterfaceSetFlags{
			SwIfIndex: afPacketCreateRsp.SwIfIndex,
			Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
		})
		if err != nil {
			log.FromContext(ctx).Fatal("set interface table ", err)
			return err
		}

		ipPrefix, err := ip_types.ParseAddressWithPrefix(interfaceAddress)
		if err != nil {
			log.FromContext(ctx).Fatal("parse ip address ", err)
			return err
		}

		ipAddress := &interfaces.SwInterfaceAddDelAddress{
			IsAdd:     true,
			SwIfIndex: afPacketCreateRsp.SwIfIndex,
			Prefix:    ipPrefix,
		}
		_, errx := ifaceClient.SwInterfaceAddDelAddress(ctx, ipAddress)
		if errx != nil {
			log.FromContext(ctx).Fatal("add ip address ", err)
			return err
		}

		_, er := session.NewServiceClient(vppConn).AppNamespaceAddDelV2(ctx, &session.AppNamespaceAddDelV2{
			Secret:      secret,
			SwIfIndex:   afPacketCreateRsp.SwIfIndex,
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

/* func createtempDir() string {
	dir, err := ioutil.TempDir("/tmp", "hstf-vpp-*")
	if err != nil {
		fmt.Println("error creating temp dir: ", err)
		return ""
	}
	return dir
} */

/* func checkAppExists(name string) {
    path, err := exec.LookPath(name)
    if err != nil {
        fmt.Printf("didn't find '%s' executable\n", name)
    }
} */

var addEnv string = "LD_PRELOAD=/home/vagrant/vpp/build-root/build-vpp_debug-native/vpp/lib/x86_64-linux-gnu/libvcl_ldpreload.so"

func startServerApp(done chan struct{}, env []string) {
	cmd := exec.Command("iperf3", "-4", "-s")
	if env != nil {
		cmd.Env = env
	}
	err := cmd.Start()
	if err != nil {
		log.Default().Errorf("failed to start server app: '%s'\n", err)
	}
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

type ConfFn func(context.Context, api.Connection) error

// func startVpp(tc *TcContext, ifName, interfaceAddress, namespaceId string, secret uint64) {
func startVpp(tc *TcContext, instance string, startupCofnig *Stanza, confFn ConfFn) {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer cancel()

	tc.mainCh <- cancel

	path := fmt.Sprintf("/tmp/%s", instance)
	log.EnableTracing(true)
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))

	con, vppErrCh := vpphelper.StartAndDialContext(ctx, vpphelper.WithRootDir(path),
		vpphelper.WithStanza(startupCofnig.ToString()))
	exitOnErrCh(ctx, cancel, vppErrCh)

	err := confFn(ctx, con)
	if err != nil {
		log.FromContext(ctx).Errorf("configuration failed: %s", err)
	}

	// 'notify' main thread that configuration is finished
	tc.wg.Done()

	<-ctx.Done()
	<-vppErrCh
}

// gather all cancel functions
func receiveCancelFns(tc *TcContext, n int) []context.CancelFunc {
	var res []context.CancelFunc
	for i := 0; i < n; i++ {
		res = append(res, <-tc.mainCh)
	}
	return res
}

// TODO: fix crash when linux topo isn't configured

type UninitFunc func()

func configureTopo(peer1, peer2 string) ([]UninitFunc, error) {
	var fns []UninitFunc
	var peerIfNames []string
	const brname = "hsbr"
	const ns = "hsns"

	peer12, err := AddVethPair(peer1)
	if err != nil {
		return fns, err
	}
	fns = append(fns, func() { DelLink(peer1) })

	peer22, err := AddVethPair(peer2)
	if err != nil {
		return fns, err
	}
	fns = append(fns, func() { DelLink(peer2) })

	peerIfNames = append(peerIfNames, peer12)
	peerIfNames = append(peerIfNames, peer22)

	err = AddNamespace(ns)
	if err != nil {
		return fns, err
	}
	err = LinkSetNamespace(peerIfNames[0], ns)
	if err != nil {
		return fns, err
	}

	// at this point we only need to delete namespace
	fns = nil
	fns = append(fns, func() { DelNamespace(ns) })

	err = LinkSetNamespace(peerIfNames[1], ns)
	if err != nil {
		return fns, err
	}

	err = AddBridge(brname, peerIfNames, ns)
	if err != nil {
		return fns, err
	}

	return fns, nil
}

func cli(inst, command string) {
	cmd := exec.Command("vppctl", "-s", fmt.Sprintf("/tmp/%s/var/run/vpp/cli.sock", inst), command)
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Errorf("failed to execute command: '%s'.\n", err)
	}
	log.Default().Debugf("Command output %s", o)
}

func (tc *TcContext) init(nInst int) {
	var wg sync.WaitGroup
	wg.Add(nInst)
	tc.wg = &wg
	tc.mainCh = make(chan context.CancelFunc)
}

func runLDPreloadVpp() error {
	var tc TcContext
	tc.init(2)
	stopServerCh := make(chan struct{}, 1)

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

	cli("vppsrv", "show int")
	log.Default().Debug("attaching clients")

	srvEnv := append(os.Environ(), addEnv, "VCL_CONFIG=vcl_srv.conf")
	go startServerApp(stopServerCh, srvEnv)

	clnEnv := append(os.Environ(), addEnv, "VCL_CONFIG=vcl_cln.conf")
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

func runLDPreloadLinux() error {
	tcFinished := make(chan struct{})
	stopServerCh := make(chan struct{})

	go startServerApp(stopServerCh, nil)
	go startClientApp(nil, tcFinished)

	<-tcFinished
	stopServerCh <- struct{}{}

	return nil
}

func testLDPreloadIperfVpp() error {
	// exechelper.Run("printenv", exechelper.WithStdout(os.Stdout))
	// exechelper.Run("which vpp", exechelper.WithStdout(os.Stdout))
	unconfigFns, err := configureTopo("vppsrv", "vppcln")
	for _, v := range unconfigFns {
		defer v()
	}

	if err != nil {
		fmt.Printf("%s\n", err)
		return errors.New("error configuring network")
	}

	err = runLDPreloadVpp()

	log.Default().Debug("Test case finished.")
	return err
}

func testLDPreloadIperfLinux() error {
	unconfigFns, err := configureTopo("vppsrv", "vppcln")
	for _, v := range unconfigFns {
		defer v()
	}

	if err != nil {
		return err
	}

	const tapName = "tap0"
	err = AddTap(tapName, "10.10.10.1/24")
	if err != nil {
		return err
	}
	defer DelLink(tapName)

	err = runLDPreloadLinux()

	log.Default().Debug("Test case finished.")
	return err
}

func configureHttpTps(inst, server_ip, port string) ConfFn {
	return func(ctx context.Context,
		vppConn api.Connection) error {
		client_ip4 := "172.0.0.2"
		_, err := session.NewServiceClient(vppConn).SessionEnableDisable(ctx, &session.SessionEnableDisable{
			IsEnable: true,
		})
		if err != nil {
			return err
		}
		cli("vpp-tps", "create tap id 0 host-ip4-addr "+client_ip4+"/24")
		cli("vpp-tps", "set int ip addr tap0 "+server_ip+"/24")
		cli("vpp-tps", "set int state tap0 up")
		cli("vpp-tps", "http tps uri tcp://0.0.0.0/"+port)
		return nil
	}
}

func startCurl(finished chan error, server_ip, port string) {
	defer func() {
		finished <- errors.New("curl error")
	}()

	cmd := exec.Command("curl", "--output", "-", server_ip+":"+port+"/test_file_10M")
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Errorf("failed to start curl '%s'.\n%s", err, o)
	}
	log.Default().Debugf("Client output: %s", o)
	finished <- nil
}

func testHttpTps() error {
	finished := make(chan error, 1)
	var rc error
	var tc TcContext
	tc.init(1)
	server_ip := "172.0.0.1"
	port := "8080"

	log.Default().Debug("starting vpp..")
	go startVpp(&tc, "vpp-tps", &Stanza{}, configureHttpTps("vpp-tps", server_ip, port))
	cancelFns := receiveCancelFns(&tc, 1)

	tc.wg.Wait()

	go startCurl(finished, server_ip, port)
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

type TestFn func() error

type Test struct {
	fn   TestFn
	desc string
}

var tests []Test

func registerTestCase(fn TestFn, desc string) {
	tests = append(tests, Test{fn, desc})
}

func registerTests() {
	registerTestCase(testLDPreloadIperfVpp, "LD preload iperf (VPP)")
	registerTestCase(testLDPreloadIperfLinux, "LD preload iperf (Linux)")
	registerTestCase(testHttpTps, "HTTP tps test")
}

func printHelp() {
	fmt.Println("usage sudo ./hstf <test-number>")
	for i, v := range tests {
		fmt.Printf(" %d : %s\n", i, v.desc)
	}
}

func main() {
	rc := 0

	registerTests()

	argLength := len(os.Args[1:])
	if argLength < 1 {
		fmt.Println("arg required")
		printHelp()
		os.Exit(1)
	}

	index, err := strconv.Atoi(os.Args[1])
	if err != nil {
		fmt.Println("arg not a number")
		printHelp()
		os.Exit(1)
	}

	err = tests[index].fn()
	if err != nil {
		fmt.Printf("%s\n", err)
		rc = 1
	}
	os.Exit(rc)
}
