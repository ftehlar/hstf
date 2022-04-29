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
	// "github.com/vishvananda/netlink"
	"github.com/edwarnicke/govpp/binapi/af_packet"
	interfaces "github.com/edwarnicke/govpp/binapi/interface"
	"github.com/edwarnicke/govpp/binapi/interface_types"
	ip_types "github.com/edwarnicke/govpp/binapi/ip_types"
	"github.com/edwarnicke/govpp/binapi/session"
)

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

/*
func createAfPacket(ctx context.Context, vppConn api.Connection, link netlink.Link) (interface_types.InterfaceIndex, error) {
	afPacketCreate := &af_packet.AfPacketCreate{
		HwAddr:     types.ToVppMacAddress(&link.Attrs().HardwareAddr),
		HostIfName: link.Attrs().Name,
	}
	now := time.Now()
	afPacketCreateRsp, err := af_packet.NewServiceClient(vppConn).AfPacketCreate(ctx, afPacketCreate)
	if err != nil {
		return 0, err
	}
	log.FromContext(ctx).
		WithField("swIfIndex", afPacketCreateRsp.SwIfIndex).
		WithField("hwaddr", afPacketCreate.HwAddr).
		WithField("hostIfName", afPacketCreate.HostIfName).
		WithField("duration", time.Since(now)).
		WithField("vppapi", "AfPacketCreate").Debug("completed")

	if err := setMtu(ctx, vppConn, link, afPacketCreateRsp.SwIfIndex); err != nil {
		return 0, err
	}
	return afPacketCreateRsp.SwIfIndex, nil
}*/

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
		log.FromContext(ctx).Fatal("session enable ", err)
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
		log.Default().Fatalf("failed to start server app '%s'. Output %s", err, o)
	}
	log.Default().Debugf("command ls finished: %s", o)
}

func startClientApp(finished chan struct{}) {
	cmd := exec.Command("iperf3", "-c", "10.10.10.1", "-u", "-l", "1460", "-b", "10g")
	newEnv := append(os.Environ(), addEnv, "VCL_CONFIG=vcl_cln.conf")
	cmd.Env = newEnv
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Fatalf("failed to start server app '%s'. Output %s", err, o)
	}
	log.Default().Debugf("command ls finished: %s", o)
	finished <- struct{}{}
}

func startVpp(wg *sync.WaitGroup, ifName, interfaceAddress, namespaceId string, secret uint64) {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer cancel()

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
	if con == nil {
		fmt.Println("vpp connection is nil!")
	}

	addRunningConfig(ctx, con, ifName, interfaceAddress, namespaceId, secret)

	// 'notify' main thread that configuration is finished
	wg.Done()

	exitOnErrCh(ctx, cancel, vppErrCh)
	// <-ctx.Done()
	<-vppErrCh
}

type TcContext struct {
	wg  *sync.WaitGroup
	ctx context.Context
}

type VppConfig struct {
	ifName           string
	interfaceAddress string
	namespaceId      string
	secret           uint64
}

// TODO: fix crash when linux topo isn't configured

func main() {
	// exechelper.Run("printenv", exechelper.WithStdout(os.Stdout))
	// exechelper.Run("which vpp", exechelper.WithStdout(os.Stdout))
	var wg sync.WaitGroup
	wg.Add(2)

	finished := make(chan struct{})

	// ctx1 := TcContext{}
	// ctx1 := TcContext{}
	log.Default().Debug("starting vpps")
	go startVpp(&wg, "vpp1", "10.10.10.1/24", "1", 1)
	go startVpp(&wg, "vpp2", "10.10.10.2/24", "2", 2)

	// waiting for both vpps to finish configuration
	wg.Wait()

	log.Default().Debug("attaching clients")

	go startServerApp()
	go startClientApp(finished)
	<-finished
	log.Default().Debug("Test case finished.")
}
