package main

import (
	"context"
	"errors"
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

var addEnv string = "LD_PRELOAD=/home/vagrant/vpp/build-root/build-vpp_debug-native/vpp/lib/x86_64-linux-gnu/libvcl_ldpreload.so"

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

	Vppcli("vppsrv", "show int")
	log.Default().Debug("attaching clients")

	srvEnv := append(os.Environ(), addEnv, "VCL_CONFIG=vcl_srv.conf")
	go startServerApp(serverRunning, stopServerCh, srvEnv)

	<-serverRunning

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

func TestHttpTps() error {
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
