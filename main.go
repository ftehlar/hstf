package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"git.fd.io/govpp.git/api"
	"git.fd.io/govpp.git/binapi/session"
	"github.com/edwarnicke/exechelper"
	"github.com/edwarnicke/vpphelper"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
)

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

func writeSyncFile(rc int, msg string) error {
	syncFile := "/tmp/sync/rc"

	res := SyncResult{Code: rc, Desc: msg}
	str, err := json.Marshal(res)

	if err != nil {
		return fmt.Errorf("error marshaling result data! %v", err)
	}

	_, err = os.Open(syncFile)
	if err != nil {
		// expecting the file does not exist
		f, e := os.Create(syncFile)
		if e != nil {
			return fmt.Errorf("failed to open sync file")
		}
		defer f.Close()
		f.Write([]byte(str))
	} else {
		return fmt.Errorf("sync file exists, delete the file frst")
	}
	return nil
}

func main() {
	if len(os.Args) == 0 {
		fmt.Println("args required")
		return
	}

	// process non-vpp arguments first
	if os.Args[1] == "rm" {
		var topoBase TopoBase
		err := topoBase.LoadTopologies("topo/")
		if err != nil {
			fmt.Printf("falied to load topologies: %v\n", err)
			os.Exit(1)
		}
		topo := topoBase.FindTopoByName(os.Args[2])
		if topo == nil {
			fmt.Printf("topology %s not found", os.Args[2])
			os.Exit(1)
		}
		topo.RemoveConfig()
		os.Exit(0)
	}

	var wErr error
	// run vpp + configure
	err := processArgs()
	if err != nil {
		wErr = writeSyncFile(1, "unspecified error")
	} else {
		wErr = writeSyncFile(0, "")
	}
	if wErr != nil {
		fmt.Printf("failed to write to sync file: %v", err)
	}
}

func processArgs() error {
	if os.Args[1] == "vpp-proxy" {
		ctx, cancel := newVppContext()
		defer cancel()

		con, vppErrCh := vpphelper.StartAndDialContext(ctx, vpphelper.WithVppConfig(configTemplate))
		exitOnErrCh(ctx, cancel, vppErrCh)

		confFn := configureProxyTcp("vpp0", "10.0.0.2/24", "vpp1", "10.0.1.2/24")
		err := confFn(ctx, con)
		if err != nil {
			return fmt.Errorf("configuration failed; %v", err)
		}
		writeSyncFile(0, "")
		<-ctx.Done()

	} else if os.Args[1] == "vpp-envoy" {
		var startup Stanza
		startup.
			NewStanza("session").
			Append("enable").
			Append("use-app-socket-api").
			Append("evt_qs_memfd_seg").
			Append("event-queue-length 100000").Close()
		ctx, cancel := newVppContext()
		defer cancel()

		con, vppErrCh := vpphelper.StartAndDialContext(ctx,
			vpphelper.WithVppConfig(configTemplate+startup.ToString()),
			vpphelper.WithRootDir("/tmp/vpp-envoy"))
		exitOnErrCh(ctx, cancel, vppErrCh)

		confFn := configureProxyTcp("vpp0", "10.0.0.2/24", "vpp1", "10.0.1.2/24")
		err := confFn(ctx, con)
		if err != nil {
			return fmt.Errorf("configuration failed: %v", err)
		}
		err0 := exechelper.Run("chmod 777 -R /tmp/vpp-envoy")
		if err0 != nil {
			return fmt.Errorf("setting permissions failed: %v", err)
		}
		writeSyncFile(0, "")
		<-ctx.Done()
	} else if os.Args[1] == "http-tps" {
		ctx, cancel := newVppContext()
		defer cancel()
		con, vppErrCh := vpphelper.StartAndDialContext(ctx,
			vpphelper.WithVppConfig(configTemplate))
		exitOnErrCh(ctx, cancel, vppErrCh)

		confFn := configureProxyTcp("vpp0", "10.0.0.2/24", "vpp1", "10.0.1.2/24")
		err := confFn(ctx, con)
		if err != nil {
			return fmt.Errorf("configuration failed: %v", err)
		}

		_, err = session.NewServiceClient(con).SessionEnableDisable(ctx, &session.SessionEnableDisable{
			IsEnable: true,
		})
		if err != nil {
			return fmt.Errorf("configuration failed: %s", err)
		}
		Vppcli("", "http tps uri tcp://0.0.0.0/8080")
		writeSyncFile(0, "")
		<-ctx.Done()
	} else if os.Args[1] == "ld-preload" {
		var startup Stanza
		startup.
			NewStanza("session").
			Append("enable").
			Append("use-app-socket-api").Close()

		ctx, cancel := newVppContext()
		defer cancel()
		con, vppErrCh := vpphelper.StartAndDialContext(ctx,
			vpphelper.WithVppConfig(configTemplate+startup.ToString()),
			vpphelper.WithRootDir(fmt.Sprintf("/tmp/%s", os.Args[1])))
		exitOnErrCh(ctx, cancel, vppErrCh)

		var fn func(context.Context, api.Connection) error
		if os.Args[2] == "srv" {
			fn = configureLDPtest("vppsrv", "10.10.10.1/24", "1", 1)
		} else {
			fn = configureLDPtest("vppcln", "10.10.10.2/24", "2", 2)
		}
		err := fn(ctx, con)
		if err != nil {
			return fmt.Errorf("configuration failed: %s", err)
		}
		writeSyncFile(0, "")
		<-ctx.Done()
	} else if os.Args[1] == "echo-server" {
		errCh := exechelper.Start("vpp_echo json server log=100 socket-name /tmp/echo-srv/var/run/app_ns_sockets/1 uri quic://10.10.10.1/12344 nthreads 1 mq-size 16384 nclients 1 quic-streams 1 time sconnect:lastbyte fifo-size 4M TX=0 RX=10G use-app-socket-api")
		select {
		case err := <-errCh:
			writeSyncFile(1, fmt.Sprintf("%v", err))
		default:
		}
		writeSyncFile(0, "")
	} else if os.Args[1] == "echo-client" {
		errCh := exechelper.Start("vpp_echo client log=100 socket-name /tmp/echo-cln/run/app_ns_sockets/2 use-app-socket-api uri quic://10.10.10.1/12344")
		select {
		case err := <-errCh:
			writeSyncFile(1, fmt.Sprintf("%v", err))
		default:
		}
		writeSyncFile(0, "")
	}
	return nil
}
