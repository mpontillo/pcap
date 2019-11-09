package pcap

import (
	"context"
	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
	"github.com/pcapme/pcap/api"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type server struct{}

// This channel will be closed when the server is gracefully stopping. Any streams in-progress
// will then also be closed.
var shuttingDown chan int

func init() {
	shuttingDown = make(chan int)
}

func (s *server) Init(ctx context.Context, in *api.InitRequest) (*api.InitReply, error) {
	log.Printf("Init(%+v)", in)
	log.Printf("GetOptionalFilter() = %+v", in.GetOptionalFilter())
	filter := in.GetFilter()
	log.Printf("GetFilter() = %+v", filter)
	err := Capture(CaptureRequest{Filter: filter, Interfaces: in.GetInterfaces()})
	return &api.InitReply{
		Success: err == nil,
	}, nil
}

func (s *server) InterfaceList(ctx context.Context, in *api.InterfaceListRequest) (*api.InterfaceListReply, error) {
	log.Printf("InterfaceList(%+v)", in)
	result := &api.InterfaceListReply{
		Success: false,
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return result, nil
	}
	resultInterfaces := make([]*api.Interface, 0, len(interfaces))
	for _, iface := range interfaces {
		isUp := iface.Flags&unix.IFF_UP != 0
		if !(isUp || in.All) {
			// Skip the interface if it it's UP, or if --all wasn't specified.
			continue
		}
		resultInterface := &api.Interface{Name: iface.Name, Up: isUp}
		resultInterface.EthernetAddresses = make([]*api.Address, 0, 8)
		resultInterface.Ipv4Addresses = make([]*api.Address, 0, 8)
		resultInterface.Ipv6Addresses = make([]*api.Address, 0, 8)
		resultInterface.EthernetAddresses = append(
			resultInterface.EthernetAddresses,
			&api.Address{
				Value: iface.HardwareAddr.String(),
			})
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			address := strings.Split(addr.String(), "/")[0]
			ip := net.ParseIP(address)
			if ip.To4() != nil {
				log.Printf("[4] [%s] ip: %+v\n", addr.String(), ip)
				// Found an IPv4 address.
				resultInterface.Ipv4Addresses = append(
					resultInterface.Ipv4Addresses,
					&api.Address{
						Value: address,
					})
			} else {
				log.Printf("[6] [%s] ip: %+v\n", addr.String(), ip)
				// Found an IPv6 address.
				resultInterface.Ipv6Addresses = append(
					resultInterface.Ipv6Addresses,
					&api.Address{
						Value: address,
					})
			}
		}
		resultInterfaces = append(resultInterfaces, resultInterface)
	}
	result.Success = true
	result.Interfaces = resultInterfaces
	return result, nil
}

type packetData struct {
	data []byte
	ci   gopacket.CaptureInfo
	err  error
}

func (s *server) LiveCapture(in *api.CaptureRequest, stream api.PCAP_LiveCaptureServer) error {
	log.Printf("LiveCapture(%+v)", in)
	inactiveHandle, err := pcap.NewInactiveHandle(in.Interface)
	defer inactiveHandle.CleanUp()
	if err != nil {
		return err
	}
	err = inactiveHandle.SetImmediateMode(in.ImmediateMode)
	if err != nil {
		return err
	}
	err = inactiveHandle.SetSnapLen(int(in.Snaplen))
	if err != nil {
		return err
	}
	bufferSize := in.BufferSizeBytes
	if bufferSize == 0 {
		bufferSize = 1024 * 1024 * 4
	}
	err = inactiveHandle.SetBufferSize(int(bufferSize))
	if err != nil {
		return err
	}
	err = inactiveHandle.SetPromisc(in.PromiscuousMode)
	if err != nil {
		return err
	}
	err = inactiveHandle.SetRFMon(in.RfMonitor)
	if err != nil {
		log.Printf("%s: %s", in.Interface, err.Error())
	}
	err = inactiveHandle.SetTimeout(time.Duration(in.TimeoutNanoseconds))
	if err != nil {
		return err
	}
	// XXX: Need to implement listing supported timestamp sources, and setting the timestamp source.
	// See also: 'man pcap_set_tstamp_type'.
	handle, err := inactiveHandle.Activate()
	if len(in.Filter) > 0 {
		err = handle.SetBPFFilter(in.Filter)
		if err != nil {
			return err
		}
	}
	defer handle.Close()
	if err != nil {
		return err
	}
	// XXX: Send over an api.CaptureHeader object.
	for {
		packet := make(chan *packetData)
		go func() {
			data, captureInfo, err := handle.ReadPacketData()
			packet <- &packetData{data, captureInfo, err}

		}()
		select {
		case _, running := <-shuttingDown:
			if running == false {
				log.Printf("Stopped LiveCapture(%+v) via interrupt.\n", in)
				return nil
			}
		case <-stream.Context().Done():
			log.Println("Context().Done()")
			// Connection closed by remote host.
			return nil
		case p := <-packet:
			if p.err != nil {
				return p.err
			}
			packetData := &api.PacketData{
				Seconds:        p.ci.Timestamp.Unix(),
				Microseconds:   uint32(p.ci.Timestamp.Nanosecond()) * 1000,
				OriginalLength: uint32(p.ci.Length),
				Data:           p.data,
			}
			err = stream.Send(&api.CaptureReply{
				ReplyData: &api.CaptureReply_Data{Data: packetData},
			})
			if err != nil {
				return err
			}
		}
	}
}

func registerSigQuitHandler() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGQUIT)
	buf := make([]byte, 1<<20)
	for {
		<-sigs
		stacklen := runtime.Stack(buf, true)
		log.Printf("=== received SIGQUIT ===\n*** goroutine dump...\n%s\n*** end\n", buf[:stacklen])
	}
}

func StartUnixSocketServer() {
	go registerSigQuitHandler()
	listener, err := net.Listen("unix", DefaultSocketPath)
	if err != nil {
		log.Fatalf("Failed to Listen(): %v", err)
	}
	// User/group permission, but not just anyone.
	if err := os.Chmod(DefaultSocketPath, 0770); err != nil {
		log.Fatal(err)
	}
	s := grpc.NewServer()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	context.WithCancel(context.Background())
	go func() {
		<-c
		log.Println("Interrupt received; stopping gracefully...")
		// Before we stop the service, we need to notify any streams that we're shutting down.
		close(shuttingDown)
		s.GracefulStop()
	}()

	api.RegisterPCAPServer(s, &server{})
	if err := s.Serve(listener); err != nil {
		log.Fatalf("Failed to Serve(): %v", err)
	}
}
