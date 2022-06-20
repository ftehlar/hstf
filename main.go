package main

import (
	"context"
	"fmt"
	"os"

	"git.fd.io/govpp.git/api"
	"git.fd.io/govpp.git/binapi/session"
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

func main() {
	if len(os.Args) == 0 {
		fmt.Println("args required")
		return
	}

	if os.Args[1] == "rm" {
		var topoBase TopoBase
		err := topoBase.LoadTopologies("topo/")
		if err != nil {
			fmt.Printf("falied to load topologies: %v\n", err)
			return
		}
		topo := topoBase.FindTopoByName(os.Args[2])
		if topo == nil {
			fmt.Printf("topology %s not found", os.Args[2])
			return
		}
		topo.RemoveConfig()
	} else if os.Args[1] == "vpp-proxy" {

		ctx, cancel := newVppContext()
		defer cancel()

		con, vppErrCh := vpphelper.StartAndDialContext(ctx, vpphelper.WithVppConfig(configTemplate))
		exitOnErrCh(ctx, cancel, vppErrCh)

		confFn := configureProxyTcp("vpp0", "10.0.0.2/24", "vpp1", "10.0.1.2/24")
		err := confFn(ctx, con)
		if err != nil {
			log.FromContext(ctx).Errorf("configuration failed: %s", err)
		}
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
			log.FromContext(ctx).Errorf("configuration failed: %v", err)
		}
		_, err0 := RunCommand([]string{"chmod", "777", "-R",
			"/tmp/vpp-envoy"}, "")
		if err0 != nil {
			log.FromContext(ctx).Errorf("setting permissions failed: %v", err)
		}
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
			log.FromContext(ctx).Errorf("configuration failed: %v", err)
		}

		_, err = session.NewServiceClient(con).SessionEnableDisable(ctx, &session.SessionEnableDisable{
			IsEnable: true,
		})
		if err != nil {
			log.FromContext(ctx).Errorf("configuration failed: %s", err)
			os.Exit(1)
		}
		Vppcli("", "http tps uri tcp://0.0.0.0/8080")
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
			vpphelper.WithRootDir("/tmp"))
		exitOnErrCh(ctx, cancel, vppErrCh)

		var fn func(context.Context, api.Connection) error
		if os.Args[2] == "srv" {
			fn = configureLDPtest("vppsrv", "10.10.10.1/24", "1", 1)
		} else {
			fn = configureLDPtest("vppcln", "10.10.10.2/24", "2", 2)
		}
		err := fn(ctx, con)
		if err != nil {
			log.FromContext(ctx).Errorf("configuration failed: %s", err)
		}
		<-ctx.Done()
	}
}
