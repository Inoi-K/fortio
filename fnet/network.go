// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fnet // import "fortio.org/fortio/fnet"

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"fortio.org/fortio/dflag"
	"fortio.org/fortio/log"
	"fortio.org/fortio/version"
)

const (
	// DefaultGRPCPort is the Fortio gRPC server default port number.
	DefaultGRPCPort = "8079"
	// StandardHTTPPort is the Standard http port number.
	StandardHTTPPort = "80"
	// StandardHTTPSPort is the Standard https port number.
	StandardHTTPSPort = "443"
	// PrefixHTTP is a constant value for representing http protocol that can be added prefix of url.
	PrefixHTTP = "http://"
	// PrefixHTTPS is a constant value for representing secure http protocol that can be added prefix of url.
	PrefixHTTPS = "https://"

	// POST is a constant value that indicates http method as post.
	POST = "POST"
	// GET is a constant value that indicates http method as get.
	GET = "GET"
	// UnixDomainSocket type for network addresses.
	UnixDomainSocket = "unix"
)

var (
	// KILOBYTE is a constant for kilobyte (ie 1024).
	KILOBYTE = 1024
	// MaxPayloadSize is the maximum size of payload to be generated by the
	// EchoHandler size= argument. In bytes.
	MaxPayloadSize = 256 * KILOBYTE
	// Payload that is returned during echo call.
	Payload []byte
	// FlagResolveIPType indicates which IP types to resolve.
	// With round robin resolution now the default, you are likely to get ipv6 which may not work if
	// use both type (`ip`). In particular some test environments like the CI do have ipv6
	// for localhost but fail to connect. So we made the default ip4 only.
	FlagResolveIPType = dflag.DynString(flag.CommandLine, "resolve-ip-type", "ip4",
		"Resolve `type`: ip4 for ipv4, ip6 for ipv6 only, use ip for both")
	// FlagResolveMethod decides which method to use when multiple ips are returned for a given name
	// default assumes one gets all the ips in the first call and does round robin across these.
	// first just picks the first answer, rr rounds robin on each answer.
	FlagResolveMethod = dflag.DynString(flag.CommandLine, "dns-method", "cached-rr",
		"When a name resolves to multiple ip, which `method` to pick: cached-rr for cached round robin, rnd for random, "+
			"first for first answer (pre 1.30 behavior), rr for round robin.").WithValidator(dnsValidator)
	// cache for cached-rr mode.
	dnsMutex sync.Mutex
	// all below are updated under lock.
	dnsHost       string
	dnsAddrs      []net.IP
	dnsRoundRobin uint32 = 0
)

func dnsValidator(inp string) error {
	valid := map[string]bool{
		"cached-rr": true,
		"rnd":       true,
		"rr":        true,
		"first":     true,
	}
	if valid[inp] {
		return nil
	}
	return fmt.Errorf("invalid value for -dns-method, should be one of cached-rr, first, rnd or rr")
}

// nolint: gochecknoinits // needed here (unit change)
func init() {
	ChangeMaxPayloadSize(MaxPayloadSize)
	rand.Seed(time.Now().UnixNano())
}

// ChangeMaxPayloadSize is used to change max payload size and fill it with pseudorandom content.
func ChangeMaxPayloadSize(newMaxPayloadSize int) {
	if newMaxPayloadSize >= 0 {
		MaxPayloadSize = newMaxPayloadSize
	} else {
		MaxPayloadSize = 0
	}
	Payload = make([]byte, MaxPayloadSize)
	// One shared and 'constant' (over time) but pseudo random content for payload
	// (to defeat compression).
	_, err := rand.Read(Payload) // nolint: gosec // We don't need crypto strength here, just low cpu and speed
	if err != nil {
		log.Errf("Error changing payload size, read for %d random payload failed: %v", newMaxPayloadSize, err)
	}
}

// NormalizePort parses port and returns host:port if port is in the form
// of host:port already or :port if port is only a port (doesn't contain :).
func NormalizePort(port string) string {
	if strings.ContainsAny(port, ":") {
		return port
	}
	return ":" + port
}

// Listen returns a listener for the port. Port can be a port or a
// bind address and a port (e.g. "8080" or "[::1]:8080"...). If the
// port component is 0 a free port will be returned by the system.
// If the port is a pathname (contains a /) a unix domain socket listener
// will be used instead of regular tcp socket.
// This logs critical on error and returns nil (is meant for servers
// that must start).
func Listen(name string, port string) (net.Listener, net.Addr) {
	sockType := "tcp"
	nPort := port
	if strings.Contains(port, "/") {
		sockType = UnixDomainSocket
	} else {
		nPort = NormalizePort(port)
	}
	listener, err := net.Listen(sockType, nPort)
	if err != nil {
		log.Critf("Can't listen to %s socket %v (%v) for %s: %v", sockType, port, nPort, name, err)
		return nil, nil
	}
	lAddr := listener.Addr()
	if len(name) > 0 {
		fmt.Printf("Fortio %s %s server listening on %s %s\n", version.Short(), name, sockType, lAddr)
	}
	return listener, lAddr
}

// UDPListen starts server on given port. (0 for dynamic port).
func UDPListen(name string, port string) (*net.UDPConn, net.Addr) {
	nPort := NormalizePort(port)
	udpAddr, err := net.ResolveUDPAddr("udp", nPort)
	if err != nil {
		log.Critf("[%v] Can't resolve UDP address %v: %v", name, nPort, err)
		return nil, nil
	}
	udpconn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Critf("[%v] Can't ListenUDP to %+v: %v", name, udpAddr, err)
		return nil, nil
	}
	if len(name) > 0 {
		fmt.Printf("Fortio %s %s server listening on udp %s\n", version.Short(), name, udpconn.LocalAddr())
	}
	return udpconn, udpconn.LocalAddr()
}

func handleTCPEchoRequest(name string, conn net.Conn) {
	SetSocketBuffers(conn, 32*KILOBYTE, 32*KILOBYTE)
	wb, err := Copy(conn, conn) // io.Copy(conn, conn)
	log.LogVf("TCP echo server (%v) echoed %d bytes from %v to itself (err=%v)", name, wb, conn.RemoteAddr(), err)
	_ = conn.Close()
}

// TCPEchoServer starts a TCP Echo Server on given port, name is for logging.
func TCPEchoServer(name string, port string) net.Addr {
	listener, addr := Listen(name, port)
	if listener == nil {
		return nil // error already logged
	}
	go func() {
		for {
			// TODO limit number of go request, maximum duration/bytes sent, etc...
			conn, err := listener.Accept()
			if err != nil {
				log.Critf("TCP echo server (%v) error accepting: %v", name, err) // will this loop with error?
			} else {
				log.LogVf("TCP echo server (%v) accepted connection from %v -> %v",
					name, conn.RemoteAddr(), conn.LocalAddr())
				go handleTCPEchoRequest(name, conn)
			}
		}
	}()
	return addr
}

func handleUDPEchoRequest(name string, conn *net.UDPConn, addr *net.UDPAddr, buf []byte) {
	wb, err := conn.WriteToUDP(buf, addr)
	log.LogVf("UDP echo server (%v) echoed %d bytes back to %v (err=%v)", name, wb, addr, err)
}

// UDPEchoServer starts a UDP Echo Server on given port, name is for logging.
// if async flag is true will spawn go routines to reply otherwise single go routine.
func UDPEchoServer(name string, port string, async bool) net.Addr {
	if async {
		name += "-async"
	}
	listener, addr := UDPListen(name, port)
	if listener == nil {
		return nil // error already logged
	}
	go func() {
		for {
			// TODO limit number of go request, maximum duration/bytes sent, etc...
			buf := make([]byte, 2048) // bigger than even IPv6 minimum MTU (~1500); 1 per thread/input
			size, conn, err := listener.ReadFromUDP(buf)
			if err != nil {
				log.Critf("UDP echo server (%v) error reading: %v", name, err)
			} else {
				log.LogVf("UDP echo server (%v) read %d from %v -> %v",
					name, size, addr, conn)
				// Synchronous or go routines
				if async {
					go handleUDPEchoRequest(name, listener, conn, buf[:size])
				} else {
					handleUDPEchoRequest(name, listener, conn, buf[:size])
				}
			}
		}
	}()
	return addr
}

// GetPort extracts the port for TCP sockets and the path for unix domain sockets.
func GetPort(lAddr net.Addr) string {
	var lPort string
	// Note: might panic if called with something else than unix or tcp socket addr, it's ok.
	if lAddr.Network() == UnixDomainSocket {
		lPort = lAddr.(*net.UnixAddr).Name
	} else if lAddr.Network() == "udp" {
		lPort = strconv.Itoa(lAddr.(*net.UDPAddr).Port)
	} else {
		lPort = strconv.Itoa(lAddr.(*net.TCPAddr).Port)
	}
	return lPort
}

// HostPortAddr is the missing base.
// IPAddr and UDPAddr are actually the same but don't share a base (!)
type HostPortAddr struct {
	IP   net.IP
	Port int
}

func (hpa *HostPortAddr) String() string {
	ipstr := hpa.IP.String()
	if strings.Contains(ipstr, ":") {
		ipstr = "[" + ipstr + "]"
	}
	return ipstr + ":" + strconv.Itoa(hpa.Port)
}

// UDPPrefix is the prefix that given to NetCat switches to UDP from TCP(/unix domain) socket type.
const UDPPrefix = "udp://"

// ResolveDestination returns the TCP address of the "host:port" suitable for net.Dial.
// nil in case of errors. Backward compatible name (1.12 and prior) for TCPResolveDestination.
func ResolveDestination(dest string) (*net.TCPAddr, error) {
	return TCPResolveDestination(dest)
}

// TCPResolveDestination returns the TCP address of the "host:port" suitable for net.Dial.
// nil in case of errors.
func TCPResolveDestination(dest string) (*net.TCPAddr, error) {
	addr, err := ResolveDestinationInternal(dest, "tcp://", "udp://")
	if err != nil {
		return nil, err
	}
	return &net.TCPAddr{IP: addr.IP, Port: addr.Port}, nil
}

// ResolveDestinationInternal returns the address of the "host:port" suitable for net.Dial.
// nil in case of errors. Works for both TCP and UDP but proto must be passed as expected == tcp:// or udp://
// and the other as unexpected.
func ResolveDestinationInternal(dest string, expected string, unexpected string) (*HostPortAddr, error) {
	if strings.HasPrefix(dest, unexpected) {
		err := fmt.Errorf("expecting %s but got %s destination %q", expected, unexpected, dest)
		log.Errf("ResolveDestination %s", err)
		return nil, err
	}
	if strings.HasPrefix(dest, expected) {
		dest = dest[len(expected):]
		dest = strings.TrimSuffix(dest, "/")
		log.Debugf("Removed %s prefix dest now %q", expected, dest)
	}
	i := strings.LastIndex(dest, ":") // important so [::1]:port works
	if i < 0 {
		log.Errf("Destination '%s' is not host:port format", dest)
		return nil, fmt.Errorf("destination '%s' is not host:port format", dest)
	}
	host := dest[0:i]
	port := dest[i+1:]
	return ResolveByProto(host, port, expected[:3]) // this could crash if not getting tcp:// -> tcp etc...
}

// Resolve backward compatible TCP only version of ResolveByProto.
func Resolve(host string, port string) (*net.TCPAddr, error) {
	addr, err := ResolveByProto(host, port, "tcp")
	if err != nil {
		return nil, err
	}
	return &net.TCPAddr{IP: addr.IP, Port: addr.Port}, nil
}

// ClearResolveCache clears the DNS cache for cached-rr resolution mode.
// For instance in case of error, to force re-resolving to potentially changed IPs.
func ClearResolveCache() {
	dnsMutex.Lock()
	dnsHost = ""
	dnsAddrs = nil
	dnsMutex.Unlock()
}

// checkCache will return true if it found and unlocked, keep the lock otherwise
func checkCache(host string) (found bool, idx uint32, res net.IP) {
	dnsMutex.Lock() // unlock before IOs
	if host != dnsHost {
		// keep the lock locked
		return
	}
	found = true
	idx = dnsRoundRobin % uint32(len(dnsAddrs))
	dnsRoundRobin++
	res = dnsAddrs[idx]
	dnsMutex.Unlock() // unlock before IOs
	log.LogVf("Resolved %s:%s to cached #%d addr %+v", host, port, idx, dest)
}

// ResolveByProto returns the address of the host,port suitable for net.Dial.
// nil in case of errors. works for both "tcp" and "udp" proto.
// Limit which address type is returned using `resolve-ip` ip4/ip6/ip (for both, default).
// If the same host is requested, and it has more than 1 IP, returned value will first,
// random or roundrobin or cached roundrobin over the ips depending on the -dns-method flag value.
func ResolveByProto(host string, port string, proto string) (*HostPortAddr, error) {
	log.Debugf("Resolve() called with host=%s port=%s proto=%s", host, port, proto)
	dest := &HostPortAddr{}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		log.Debugf("host %s looks like an IPv6, stripping []", host)
		host = host[1 : len(host)-1]
	}
	var err error
	dest.Port, err = net.LookupPort(proto, port)
	if err != nil {
		log.Errf("Unable to resolve %s port '%s' : %v", proto, port, err)
		return nil, err
	}
	isAddr := net.ParseIP(host)
	if isAddr != nil {
		dest.IP = isAddr
		log.LogVf("Resolved %s:%s already an IP as addr %+v", host, port, dest)
		return dest, nil
	}
	filter := FlagResolveIPType.Get()
	dnsMethod := FlagResolveMethod.Get()
	idx := uint32(0)
	inCache := false
	if dnsMethod == "cached-rr" {
		inCache, idx, dest.IP = checkCache(host)
		if inCache {
			return dest, nil
		}
		dnsMutex.Unlock()
	}
	addrs, err := net.DefaultResolver.LookupIP(context.Background(), filter, host)
	if err != nil {
		log.Errf("Unable to lookup '%s' : %v", host, err)
		return nil, err
	}
	l := uint32(len(addrs))
	if l > 1 {
		switch dnsMethod {
		case "cached-rr":
			// (re)check if we're the first to grab this lock (other threads may be here as well)
			inCache, idx, dest.IP = checkCache(host)
			if inCache {
				return dest, nil
			}
			// first time, first thread reaching here:
			dnsHost = host
			dnsAddrs = addrs
			idx = 0
			dnsRoundRobin = 1 // next one after 0
			dnsMutex.Unlock()
			log.Debugf("First time/new host for caching address for %s : %v", host, addrs)
		case "rr":
			idx = dnsRoundRobin % uint32(len(addrs))
			dnsRoundRobin++
			log.Debugf("Using rr address #%d for %s : %v", idx, host, addrs)
		case "first":
			log.Debugf("Using first address for %s : %v", host, addrs)
		case "rnd":
			idx = uint32(rand.Intn(int(l)))
			log.Debugf("Using rnd address #%d for %s : %v", idx, host, addrs)
		}
	}
	dest.IP = addrs[idx]
	log.LogVf("Resolved %s:%s to %s %s %s #%d addr %+v", host, port, proto, filter, dnsMethod, idx, dest)
	return dest, nil
}

// UDPResolveDestination returns the UDP address of the "host:port" suitable for net.Dial.
// nil and the error in case of errors.
func UDPResolveDestination(dest string) (*net.UDPAddr, error) {
	addr, err := ResolveDestinationInternal(dest, "udp://", "tcp://")
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: addr.IP, Port: addr.Port}, nil
}

// Copy is a debug version of io.Copy without the zero Copy optimizations.
func Copy(dst io.Writer, src io.Reader) (written int64, err error) {
	buf := make([]byte, 32*KILOBYTE)
	for {
		nr, er := src.Read(buf)
		log.Debugf("read %d from %+v: %v", nr, src, er)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			log.Debugf("wrote %d (expected %d) to %+v: %v", nw, nr, dst, ew)
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				log.Errf("copy: %+v -> %+v write error: %v", src, dst, ew)
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if os.IsTimeout(er) {
				// return but not log as error (for UDPNetCat use case)
				err = er
				log.LogVf("copy: %+v -> %+v timeout/read error: %v", src, dst, er)
			} else if !errors.Is(er, io.EOF) {
				err = er
				log.Errf("copy: %+v -> %+v read error: %v", src, dst, er)
			}
			break
		}
	}
	return written, err
}

// SetSocketBuffers sets the read and write buffer size of the socket. Also sets tcp SetNoDelay().
func SetSocketBuffers(socket net.Conn, readBufferSize, writeBufferSize int) {
	tcpSock, ok := socket.(*net.TCPConn)
	if !ok {
		log.LogVf("Not setting socket options on non tcp socket %v", socket.RemoteAddr())
		return
	}
	// For now those errors are not critical/breaking
	if err := tcpSock.SetNoDelay(true); err != nil {
		log.Warnf("Unable to connect to set tcp no delay %+v: %v", socket, err)
	}
	if err := tcpSock.SetWriteBuffer(writeBufferSize); err != nil {
		log.Warnf("Unable to connect to set write buffer %d %+v: %v", writeBufferSize, socket, err)
	}
	if err := tcpSock.SetReadBuffer(readBufferSize); err != nil {
		log.Warnf("Unable to connect to read buffer %d %+v: %v", readBufferSize, socket, err)
	}
}

func transfer(wg *sync.WaitGroup, dst net.Conn, src net.Conn) {
	n, oErr := io.Copy(dst, src) // keep original error for logs below
	log.LogVf("Proxy: transferred %d bytes from %v to %v (err=%v)", n, src.RemoteAddr(), dst.RemoteAddr(), oErr)
	sTCP, ok := src.(*net.TCPConn)
	if ok {
		err := sTCP.CloseRead()
		if err != nil { // We got an eof so it's already half closed.
			log.LogVf("Proxy: semi expected error CloseRead on src %v: %v,%v", src.RemoteAddr(), err, oErr)
		}
	}
	dTCP, ok := dst.(*net.TCPConn)
	if ok {
		err := dTCP.CloseWrite()
		if err != nil {
			log.Errf("Proxy: error CloseWrite on dst %v: %v,%v", dst.RemoteAddr(), err, oErr)
		}
	}
	wg.Done()
}

// ErrNilDestination returned when trying to proxy to a nil address.
var ErrNilDestination = fmt.Errorf("nil destination")

func handleProxyRequest(conn net.Conn, dest net.Addr) {
	err := ErrNilDestination
	var d net.Conn
	if dest != nil {
		d, err = net.Dial(dest.Network(), dest.String())
	}
	if err != nil {
		log.Errf("Proxy: unable to connect to %v for %v : %v", dest, conn.RemoteAddr(), err)
		_ = conn.Close()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2) // 2 threads to wait for...
	go transfer(&wg, d, conn)
	transfer(&wg, conn, d)
	wg.Wait()
	log.LogVf("Proxy: both sides of transfer to %v for %v done", dest, conn.RemoteAddr())
	// Not checking as we are closing/ending anyway - note: bad side effect of coverage...
	_ = d.Close()
	_ = conn.Close()
}

// Proxy starts a tcp proxy.
func Proxy(port string, dest net.Addr) net.Addr {
	listener, lAddr := Listen(fmt.Sprintf("proxy for %v", dest), port)
	if listener == nil {
		return nil // error already logged
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Critf("Proxy: error accepting: %v", err) // will this loop with error?
			} else {
				log.LogVf("Proxy: Accepted proxy connection from %v -> %v (for listener %v)",
					conn.RemoteAddr(), conn.LocalAddr(), dest)
				// TODO limit number of go request, use worker pool, etc...
				go handleProxyRequest(conn, dest)
			}
		}
	}()
	return lAddr
}

// ProxyToDestination opens a proxy from the listenPort (or addr:port or unix domain socket path) and forwards
// all traffic to destination (host:port).
func ProxyToDestination(listenPort string, destination string) net.Addr {
	addr, _ := TCPResolveDestination(destination)
	return Proxy(listenPort, addr)
}

// NormalizeHostPort generates host:port string for the address or uses localhost instead of [::]
// when the original port binding input didn't specify an address.
func NormalizeHostPort(inputPort string, addr net.Addr) string {
	urlHostPort := addr.String()
	if addr.Network() == UnixDomainSocket {
		urlHostPort = fmt.Sprintf("-unix-socket=%s", urlHostPort)
	} else {
		if strings.HasPrefix(inputPort, ":") || !strings.Contains(inputPort, ":") {
			urlHostPort = fmt.Sprintf("localhost:%d", addr.(*net.TCPAddr).Port)
		}
	}
	return urlHostPort
}

// ValidatePayloadSize compares input size with MaxPayLoadSize. If size exceeds the MaxPayloadSize
// size will set to MaxPayLoadSize.
func ValidatePayloadSize(size *int) {
	if *size > MaxPayloadSize && *size > 0 {
		log.Warnf("Requested size %d greater than max size %d, using max instead (change max using -maxpayloadsizekb)",
			*size, MaxPayloadSize)
		*size = MaxPayloadSize
	} else if *size < 0 {
		log.Warnf("Requested size %d is negative, using 0 (no additional payload) instead.", *size)
		*size = 0
	}
}

// GenerateRandomPayload generates a random payload with given input size.
func GenerateRandomPayload(payloadSize int) []byte {
	ValidatePayloadSize(&payloadSize)
	return Payload[:payloadSize]
}

// ReadFileForPayload reads the file from given input path.
func ReadFileForPayload(payloadFilePath string) ([]byte, error) {
	data, err := ioutil.ReadFile(payloadFilePath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// GeneratePayload generates a payload with given inputs.
// First tries filePath, then random payload, at last payload.
func GeneratePayload(payloadFilePath string, payloadSize int, payload string) []byte {
	if len(payloadFilePath) > 0 {
		p, err := ReadFileForPayload(payloadFilePath)
		if err != nil {
			log.Warnf("File read operation is failed %v", err)
			return nil
		}
		return p
	} else if payloadSize > 0 {
		return GenerateRandomPayload(payloadSize)
	} else {
		return []byte(payload)
	}
}

// GetUniqueUnixDomainPath returns a path to be used for unix domain socket.
func GetUniqueUnixDomainPath(prefix string) string {
	if prefix == "" {
		prefix = "fortio-uds"
	}
	f, err := ioutil.TempFile(os.TempDir(), prefix)
	if err != nil {
		log.Errf("Unable to generate temp file with prefix %s: %v", prefix, err)
		return "/tmp/fortio-default-uds"
	}
	fname := f.Name()
	_ = f.Close()
	// for the bind to succeed we need the file to not pre exist:
	_ = os.Remove(fname)
	return fname
}

// SmallReadUntil will read one byte at a time until stopByte is found and up to max bytes total.
// Returns what was read (without the stop byte when found), whether the stop byte was found, whether an error occurred (eof...).
// Because we read one by one directly (no buffer) this should only be used for short variable length preamble type read.
func SmallReadUntil(r io.Reader, stopByte byte, max int) ([]byte, bool, error) {
	buf := make([]byte, max)
	i := 0
	for i < max {
		n, err := r.Read(buf[i : i+1])
		if err != nil {
			return buf[0:i], false, err
		}
		if n != 1 {
			log.Critf("Bug/unexpected case, read %d instead of 1 byte yet no error", n)
		}
		if buf[i] == stopByte {
			return buf[0:i], true, nil
		}
		i += n
	}
	return buf[0:i], false, nil
}

// NetCat connects to the destination and reads from in, sends to the socket, and write what it reads from the socket to out.
// if the destination starts with udp:// UDP is used otherwise TCP.
func NetCat(dest string, in io.Reader, out io.Writer, stopOnEOF bool) error {
	if strings.HasPrefix(dest, UDPPrefix) {
		return UDPNetCat(dest, in, out, stopOnEOF)
	}
	log.Infof("TCP NetCat to %s, stop on eof %v", dest, stopOnEOF)
	a, err := TCPResolveDestination(dest)
	if a == nil {
		return err // already logged
	}
	d, err := net.DialTCP("tcp", nil, a)
	if err != nil {
		log.Errf("Connection error to %q: %v", dest, err)
		return err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var wb int64
	var we error
	go func(w *sync.WaitGroup, src io.Reader, dst *net.TCPConn) {
		wb, we = Copy(dst, src)
		_ = dst.CloseWrite()
		w.Done()
	}(&wg, in, d)
	rb, re := Copy(out, d)
	log.Infof("Read %d from %s (err=%v)", rb, dest, re)
	if !stopOnEOF {
		wg.Wait()
	}
	log.Infof("Wrote %d to %s (err=%v)", wb, dest, we)
	_ = d.Close()
	if c, ok := in.(io.Closer); ok {
		_ = c.Close()
	}
	if c, ok := out.(io.Closer); ok {
		_ = c.Close()
	}
	if re != nil {
		return re
	}
	if we != nil {
		return we
	}
	return nil
}

// UDPNetCat handles UDP part of NetCat.
func UDPNetCat(dest string, in io.Reader, out io.Writer, stopOnEOF bool) error {
	log.Infof("UDP NetCat to %s, stop on eof %v", dest, stopOnEOF)
	a, err := UDPResolveDestination(dest)
	if a == nil {
		return err // already logged
	}
	d, err := net.DialUDP("udp", nil, a)
	if err != nil {
		log.Errf("Connection error to %q: %v", dest, err)
		return err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var rb int64
	var re error
	go func(w *sync.WaitGroup, dst io.Writer, src io.Reader) {
		rb, re = Copy(dst, src)
		w.Done()
	}(&wg, out, d)
	wb, we := Copy(d, in)
	_ = d.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	wg.Wait()
	log.Infof("Read %d, Wrote %d bytes to UDP %v (re %v we %v)", rb, wb, a, re, we)
	return err
}

// EscapeBytes returns printable string. Same as %q format without the
// surrounding/extra "".
func EscapeBytes(buf []byte) string {
	e := fmt.Sprintf("%q", buf)
	return e[1 : len(e)-1]
}

// DebugSummary returns a string with the size and escaped first max/2 and
// last max/2 bytes of a buffer (or the whole escaped buffer if small enough).
func DebugSummary(buf []byte, max int) string {
	l := len(buf)
	if l <= max+3 { // no point in shortening to add ... if we could return those 3
		return EscapeBytes(buf)
	}
	max /= 2
	return fmt.Sprintf("%d: %s...%s", l, EscapeBytes(buf[:max]), EscapeBytes(buf[l-max:]))
}
