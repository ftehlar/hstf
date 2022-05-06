package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

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

func addRunningConfig(ctx context.Context,
	vppConn api.Connection,
	ifName, interfaceAddress, namespaceId string,
	secret uint64) (interface_types.InterfaceIndex, error) {

	ifaceClient := interfaces.NewServiceClient(vppConn)
	afPacketCreate := &af_packet.AfPacketCreateV2{
		UseRandomHwAddr: true,
		HostIfName:      ifName,
		NumRxQueues:     1,
	}
	afPacketCreateRsp, err := af_packet.NewServiceClient(vppConn).AfPacketCreateV2(ctx, afPacketCreate)
	if err != nil {
		log.FromContext(ctx).Fatal("failed to create af packet: ", err)
		return 0, err
	}
	_, err = ifaceClient.SwInterfaceSetFlags(ctx, &interfaces.SwInterfaceSetFlags{
		SwIfIndex: afPacketCreateRsp.SwIfIndex,
		Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
	})
	if err != nil {
		log.FromContext(ctx).Fatal("set interface table ", err)
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

	_, er := session.NewServiceClient(vppConn).AppNamespaceAddDelV2(ctx, &session.AppNamespaceAddDelV2{
		Secret:      secret,
		SwIfIndex:   afPacketCreateRsp.SwIfIndex,
		NamespaceID: namespaceId,
	})
	if er != nil {
		log.FromContext(ctx).Fatal("add app namespace ", err)
		return 0, err
	}

	_, er1 := session.NewServiceClient(vppConn).SessionEnableDisable(ctx, &session.SessionEnableDisable{
		IsEnable: true,
	})
	if er1 != nil {
		log.FromContext(ctx).Fatalf("session enable %w", err)
		return 0, err
	}

	return afPacketCreateRsp.SwIfIndex, nil
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

func startServerApp() {
	cmd := exec.Command("iperf3", "-4", "-s")
	newEnv := append(os.Environ(), addEnv, "VCL_CONFIG=vcl_srv.conf")
	cmd.Env = newEnv
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Errorf("failed to start server app '%s'. \n%s", err, o)
	}
	log.Default().Debugf("Server output: %s", o)
}

func startClientApp(finished chan struct{}) {
	defer func() {
		finished <- struct{}{}
	}()

	cmd := exec.Command("iperf3", "-c", "10.10.10.1", "-u", "-l", "1460", "-b", "10g")
	newEnv := append(os.Environ(), addEnv, "VCL_CONFIG=vcl_cln.conf")
	cmd.Env = newEnv
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Errorf("failed to start client app '%s'.\n%s", err, o)
	}
	log.Default().Debugf("Client output: %s", o)
}

func startVpp(tc *TcContext, ifName, interfaceAddress, namespaceId string, secret uint64) {
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

	path := fmt.Sprintf("/tmp/%s", ifName)
	log.EnableTracing(true)
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))

	var stanza Stanza
	stanza.
		NewStanza("session").
		Append("enable").
		Append("use-app-socket-api").Close()
	fmt.Println(stanza.ToString())

	con, vppErrCh := vpphelper.StartAndDialContext(ctx, vpphelper.WithRootDir(path),
		vpphelper.WithStanza(stanza.ToString()))
	exitOnErrCh(ctx, cancel, vppErrCh)

	addRunningConfig(ctx, con, ifName, interfaceAddress, namespaceId, secret)

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

func runTest() int {
	var tc TcContext
	tc.mainCh = make(chan context.CancelFunc)
	var wg sync.WaitGroup
	wg.Add(2)
	tc.wg = &wg

	finished := make(chan struct{})

	log.Default().Debug("starting vpps")
	go startVpp(&tc, "vppsrv", "10.10.10.1/24", "1", 1)
	go startVpp(&tc, "vppcln", "10.10.10.2/24", "2", 2)

	cancelFns := receiveCancelFns(&tc, 2)

	// waiting for both vpps to finish configuration
	wg.Wait()

	cli("vppsrv", "show int")
	log.Default().Debug("attaching clients")

	go startServerApp()
	go startClientApp(finished)
	<-finished

	// stop vpp routines
	for _, fn := range cancelFns {
		fn()
	}
	return 0
}

func testLDPreloadIperf() int {
	// exechelper.Run("printenv", exechelper.WithStdout(os.Stdout))
	// exechelper.Run("which vpp", exechelper.WithStdout(os.Stdout))
	unconfigFns, err := configureTopo("vppsrv", "vppcln")
	for _, v := range unconfigFns {
		defer v()
	}

	if err != nil {
		fmt.Printf("%s\n", err)
		return 1
	}

	rc := runTest()

	log.Default().Debug("Test case finished.")
	return rc
}

func main() {
	os.Exit(testLDPreloadIperf())
}
