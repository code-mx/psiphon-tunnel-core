/*
 * Copyright (c) 2015, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

// for HTTPSServer.ServeTLS:
/*
Copyright (c) 2012 The Go Authors. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

package psiphon

import (
	"container/list"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Inc/dns"
	"github.com/Psiphon-Inc/ratelimit"
)

const DNS_PORT = 53

// DialConfig contains parameters to determine the behavior
// of a Psiphon dialer (TCPDial, MeekDial, etc.)
type DialConfig struct {

	// UpstreamProxyUrl specifies a proxy to connect through.
	// E.g., "http://proxyhost:8080"
	//       "socks5://user:password@proxyhost:1080"
	//       "socks4a://proxyhost:1080"
	//       "http://NTDOMAIN\NTUser:password@proxyhost:3375"
	//
	// Certain tunnel protocols require HTTP CONNECT support
	// when a HTTP proxy is specified. If CONNECT is not
	// supported, those protocols will not connect.
	UpstreamProxyUrl string

	// UpstreamProxyCustomHeader is a set of additional arbitrary HTTP headers that are
	// added to all HTTP requests made through the upstream proxy specified by UpstreamProxyUrl
	// in case of HTTP proxy
	UpstreamProxyCustomHeaders http.Header

	ConnectTimeout time.Duration

	// PendingConns is used to track and interrupt dials in progress.
	// Dials may be interrupted using PendingConns.CloseAll(). Once instantiated,
	// a conn is added to pendingConns before the network connect begins and
	// removed from pendingConns once the connect succeeds or fails.
	// May be nil.
	PendingConns *Conns

	// BindToDevice parameters are used to exclude connections and
	// associated DNS requests from VPN routing.
	// When DeviceBinder is set, any underlying socket is
	// submitted to the device binding servicebefore connecting.
	// The service should bind the socket to a device so that it doesn't route
	// through a VPN interface. This service is also used to bind UDP sockets used
	// for DNS requests, in which case DnsServerGetter is used to get the
	// current active untunneled network DNS server.
	DeviceBinder    DeviceBinder
	DnsServerGetter DnsServerGetter

	// UseIndistinguishableTLS specifies whether to try to use an
	// alternative stack for TLS. From a circumvention perspective,
	// Go's TLS has a distinct fingerprint that may be used for blocking.
	// Only applies to TLS connections.
	UseIndistinguishableTLS bool

	// TrustedCACertificatesFilename specifies a file containing trusted
	// CA certs. The file contents should be compatible with OpenSSL's
	// SSL_CTX_load_verify_locations.
	// Only applies to UseIndistinguishableTLS connections.
	TrustedCACertificatesFilename string

	// DeviceRegion is the reported region the host device is running in.
	// When set, this value may be used, pre-connection, to select performance
	// or circumvention optimization strategies for the given region.
	DeviceRegion string

	// ResolvedIPCallback, when set, is called with the IP address that was
	// dialed. This is either the specified IP address in the dial address,
	// or the resolved IP address in the case where the dial address is a
	// domain name.
	// The callback may be invoked by a concurrent goroutine.
	ResolvedIPCallback func(string)
}

// NetworkConnectivityChecker defines the interface to the external
// HasNetworkConnectivity provider
type NetworkConnectivityChecker interface {
	// TODO: change to bool return value once gobind supports that type
	HasNetworkConnectivity() int
}

// DeviceBinder defines the interface to the external BindToDevice provider
type DeviceBinder interface {
	BindToDevice(fileDescriptor int) error
}

// DnsServerGetter defines the interface to the external GetDnsServer provider
type DnsServerGetter interface {
	GetPrimaryDnsServer() string
	GetSecondaryDnsServer() string
}

// HostNameTransformer defines the interface for pluggable hostname
// transformation circumvention strategies.
type HostNameTransformer interface {
	TransformHostName(hostname string) (string, bool)
}

// IdentityHostNameTransformer is the default HostNameTransformer, which
// returns the hostname unchanged.
type IdentityHostNameTransformer struct{}

func (IdentityHostNameTransformer) TransformHostName(hostname string) (string, bool) {
	return hostname, false
}

// TimeoutError implements the error interface
type TimeoutError struct{}

func (TimeoutError) Error() string   { return "timed out" }
func (TimeoutError) Timeout() bool   { return true }
func (TimeoutError) Temporary() bool { return true }

// Dialer is a custom dialer compatible with http.Transport.Dial.
type Dialer func(string, string) (net.Conn, error)

// Conns is a synchronized list of Conns that is used to coordinate
// interrupting a set of goroutines establishing connections, or
// close a set of open connections, etc.
// Once the list is closed, no more items may be added to the
// list (unless it is reset).
type Conns struct {
	mutex    sync.Mutex
	isClosed bool
	conns    map[net.Conn]bool
}

func (conns *Conns) Reset() {
	conns.mutex.Lock()
	defer conns.mutex.Unlock()
	conns.isClosed = false
	conns.conns = make(map[net.Conn]bool)
}

func (conns *Conns) Add(conn net.Conn) bool {
	conns.mutex.Lock()
	defer conns.mutex.Unlock()
	if conns.isClosed {
		return false
	}
	if conns.conns == nil {
		conns.conns = make(map[net.Conn]bool)
	}
	conns.conns[conn] = true
	return true
}

func (conns *Conns) Remove(conn net.Conn) {
	conns.mutex.Lock()
	defer conns.mutex.Unlock()
	delete(conns.conns, conn)
}

func (conns *Conns) CloseAll() {
	conns.mutex.Lock()
	defer conns.mutex.Unlock()
	conns.isClosed = true
	for conn, _ := range conns.conns {
		conn.Close()
	}
	conns.conns = make(map[net.Conn]bool)
}

// LRUConns is a concurrency-safe list of net.Conns ordered
// by recent activity. Its purpose is to facilitate closing
// the oldest connection in a set of connections.
//
// New connections added are referenced by a LRUConnsEntry,
// which is used to Touch() active connections, which
// promotes them to the front of the order and to Remove()
// connections that are no longer LRU candidates.
//
// CloseOldest() will remove the oldest connection from the
// list and call net.Conn.Close() on the connection.
//
// After an entry has been removed, LRUConnsEntry Touch()
// and Remove() will have no effect.
type LRUConns struct {
	mutex sync.Mutex
	list  *list.List
}

// NewLRUConns initializes a new LRUConns.
func NewLRUConns() *LRUConns {
	return &LRUConns{list: list.New()}
}

// Add inserts a net.Conn as the freshest connection
// in a LRUConns and returns an LRUConnsEntry to be
// used to freshen the connection or remove the connection
// from the LRU list.
func (conns *LRUConns) Add(conn net.Conn) *LRUConnsEntry {
	conns.mutex.Lock()
	defer conns.mutex.Unlock()
	return &LRUConnsEntry{
		lruConns: conns,
		element:  conns.list.PushFront(conn),
	}
}

// CloseOldest closes the oldest connection in a
// LRUConns. It calls net.Conn.Close() on the
// connection.
func (conns *LRUConns) CloseOldest() {
	conns.mutex.Lock()
	oldest := conns.list.Back()
	conn, ok := oldest.Value.(net.Conn)
	if oldest != nil {
		conns.list.Remove(oldest)
	}
	// Release mutex before closing conn
	conns.mutex.Unlock()
	if ok {
		conn.Close()
	}
}

// LRUConnsEntry is an entry in a LRUConns list.
type LRUConnsEntry struct {
	lruConns *LRUConns
	element  *list.Element
}

// Remove deletes the connection referenced by the
// LRUConnsEntry from the associated LRUConns.
// Has no effect if the entry was not initialized
// or previously removed.
func (entry *LRUConnsEntry) Remove() {
	if entry.lruConns == nil || entry.element == nil {
		return
	}
	entry.lruConns.mutex.Lock()
	defer entry.lruConns.mutex.Unlock()
	entry.lruConns.list.Remove(entry.element)
}

// Touch promotes the connection referenced by the
// LRUConnsEntry to the front of the associated LRUConns.
// Has no effect if the entry was not initialized
// or previously removed.
func (entry *LRUConnsEntry) Touch() {
	if entry.lruConns == nil || entry.element == nil {
		return
	}
	entry.lruConns.mutex.Lock()
	defer entry.lruConns.mutex.Unlock()
	entry.lruConns.list.MoveToFront(entry.element)
}

// LocalProxyRelay sends to remoteConn bytes received from localConn,
// and sends to localConn bytes received from remoteConn.
func LocalProxyRelay(proxyType string, localConn, remoteConn net.Conn) {
	copyWaitGroup := new(sync.WaitGroup)
	copyWaitGroup.Add(1)
	go func() {
		defer copyWaitGroup.Done()
		_, err := io.Copy(localConn, remoteConn)
		if err != nil {
			err = fmt.Errorf("Relay failed: %s", ContextError(err))
			NoticeLocalProxyError(proxyType, err)
		}
	}()
	_, err := io.Copy(remoteConn, localConn)
	if err != nil {
		err = fmt.Errorf("Relay failed: %s", ContextError(err))
		NoticeLocalProxyError(proxyType, err)
	}
	copyWaitGroup.Wait()
}

// WaitForNetworkConnectivity uses a NetworkConnectivityChecker to
// periodically check for network connectivity. It returns true if
// no NetworkConnectivityChecker is provided (waiting is disabled)
// or when NetworkConnectivityChecker.HasNetworkConnectivity()
// indicates connectivity. It waits and polls the checker once a second.
// If any stop is broadcast, false is returned immediately.
func WaitForNetworkConnectivity(
	connectivityChecker NetworkConnectivityChecker, stopBroadcasts ...<-chan struct{}) bool {
	if connectivityChecker == nil || 1 == connectivityChecker.HasNetworkConnectivity() {
		return true
	}
	NoticeInfo("waiting for network connectivity")
	ticker := time.NewTicker(1 * time.Second)
	for {
		if 1 == connectivityChecker.HasNetworkConnectivity() {
			return true
		}

		selectCases := make([]reflect.SelectCase, 1+len(stopBroadcasts))
		selectCases[0] = reflect.SelectCase{
			Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ticker.C)}
		for i, stopBroadcast := range stopBroadcasts {
			selectCases[i+1] = reflect.SelectCase{
				Dir: reflect.SelectRecv, Chan: reflect.ValueOf(stopBroadcast)}
		}

		chosen, _, ok := reflect.Select(selectCases)
		if chosen == 0 && ok {
			// Ticker case, so check again
		} else {
			// Stop case
			return false
		}
	}
}

// ResolveIP uses a custom dns stack to make a DNS query over the
// given TCP or UDP conn. This is used, e.g., when we need to ensure
// that a DNS connection bypasses a VPN interface (BindToDevice) or
// when we need to ensure that a DNS connection is tunneled.
// Caller must set timeouts or interruptibility as required for conn.
func ResolveIP(host string, conn net.Conn) (addrs []net.IP, ttls []time.Duration, err error) {

	// Send the DNS query
	dnsConn := &dns.Conn{Conn: conn}
	defer dnsConn.Close()
	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(host), dns.TypeA)
	query.RecursionDesired = true
	dnsConn.WriteMsg(query)

	// Process the response
	response, err := dnsConn.ReadMsg()
	if err != nil {
		return nil, nil, ContextError(err)
	}
	addrs = make([]net.IP, 0)
	ttls = make([]time.Duration, 0)
	for _, answer := range response.Answer {
		if a, ok := answer.(*dns.A); ok {
			addrs = append(addrs, a.A)
			ttl := time.Duration(a.Hdr.Ttl) * time.Second
			ttls = append(ttls, ttl)
		}
	}
	return addrs, ttls, nil
}

// MakeUntunneledHttpsClient returns a net/http.Client which is
// configured to use custom dialing features -- including BindToDevice,
// UseIndistinguishableTLS, etc. -- for a specific HTTPS request URL.
// If verifyLegacyCertificate is not nil, it's used for certificate
// verification.
// Because UseIndistinguishableTLS requires a hack to work with
// net/http, MakeUntunneledHttpClient may return a modified request URL
// to be used. Callers should always use this return value to make
// requests, not the input value.
func MakeUntunneledHttpsClient(
	dialConfig *DialConfig,
	verifyLegacyCertificate *x509.Certificate,
	requestUrl string,
	requestTimeout time.Duration) (*http.Client, string, error) {

	// Change the scheme to "http"; otherwise http.Transport will try to do
	// another TLS handshake inside the explicit TLS session. Also need to
	// force an explicit port, as the default for "http", 80, won't talk TLS.

	urlComponents, err := url.Parse(requestUrl)
	if err != nil {
		return nil, "", ContextError(err)
	}

	urlComponents.Scheme = "http"
	host, port, err := net.SplitHostPort(urlComponents.Host)
	if err != nil {
		// Assume there's no port
		host = urlComponents.Host
		port = ""
	}
	if port == "" {
		port = "443"
	}
	urlComponents.Host = net.JoinHostPort(host, port)

	// Note: IndistinguishableTLS mode doesn't support VerifyLegacyCertificate
	useIndistinguishableTLS := dialConfig.UseIndistinguishableTLS && verifyLegacyCertificate == nil

	dialer := NewCustomTLSDialer(
		// Note: when verifyLegacyCertificate is not nil, some
		// of the other CustomTLSConfig is overridden.
		&CustomTLSConfig{
			Dial: NewTCPDialer(dialConfig),
			VerifyLegacyCertificate:       verifyLegacyCertificate,
			SNIServerName:                 host,
			SkipVerify:                    false,
			UseIndistinguishableTLS:       useIndistinguishableTLS,
			TrustedCACertificatesFilename: dialConfig.TrustedCACertificatesFilename,
		})

	transport := &http.Transport{
		Dial: dialer,
	}
	httpClient := &http.Client{
		Timeout:   requestTimeout,
		Transport: transport,
	}

	return httpClient, urlComponents.String(), nil
}

// MakeTunneledHttpClient returns a net/http.Client which is
// configured to use custom dialing features including tunneled
// dialing and, optionally, UseTrustedCACertificatesForStockTLS.
// Unlike MakeUntunneledHttpsClient and makePsiphonHttpsClient,
// This http.Client uses stock TLS and no scheme transformation
// hack is required.
func MakeTunneledHttpClient(
	config *Config,
	tunnel *Tunnel,
	requestTimeout time.Duration) (*http.Client, error) {

	tunneledDialer := func(_, addr string) (conn net.Conn, err error) {
		return tunnel.sshClient.Dial("tcp", addr)
	}

	transport := &http.Transport{
		Dial: tunneledDialer,
		ResponseHeaderTimeout: requestTimeout,
	}

	if config.UseTrustedCACertificatesForStockTLS {
		if config.TrustedCACertificatesFilename == "" {
			return nil, ContextError(errors.New(
				"UseTrustedCACertificatesForStockTLS requires TrustedCACertificatesFilename"))
		}
		rootCAs := x509.NewCertPool()
		certData, err := ioutil.ReadFile(config.TrustedCACertificatesFilename)
		if err != nil {
			return nil, ContextError(err)
		}
		rootCAs.AppendCertsFromPEM(certData)
		transport.TLSClientConfig = &tls.Config{RootCAs: rootCAs}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
	}, nil
}

// MakeDownloadHttpClient is a resusable helper that sets up a
// http.Client for use either untunneled or through a tunnel.
// See MakeUntunneledHttpsClient for a note about request URL
// rewritting.
func MakeDownloadHttpClient(
	config *Config,
	tunnel *Tunnel,
	untunneledDialConfig *DialConfig,
	requestUrl string,
	requestTimeout time.Duration) (*http.Client, string, error) {

	var httpClient *http.Client
	var err error

	if tunnel != nil {
		httpClient, err = MakeTunneledHttpClient(config, tunnel, requestTimeout)
		if err != nil {
			return nil, "", ContextError(err)
		}
	} else {
		httpClient, requestUrl, err = MakeUntunneledHttpsClient(
			untunneledDialConfig, nil, requestUrl, requestTimeout)
		if err != nil {
			return nil, "", ContextError(err)
		}
	}

	return httpClient, requestUrl, nil
}

// ResumeDownload is a resuable helper that downloads requestUrl via the
// httpClient, storing the result in downloadFilename when the download is
// complete. Intermediate, partial downloads state is stored in
// downloadFilename.part and downloadFilename.part.etag.
// Any existing downloadFilename file will be overwritten.
//
// In the case where the remote object has change while a partial download
// is to be resumed, the partial state is reset and resumeDownload fails.
// The caller must restart the download.
//
// When ifNoneMatchETag is specified, no download is made if the remote
// object has the same ETag. ifNoneMatchETag has an effect only when no
// partial download is in progress.
//
func ResumeDownload(
	httpClient *http.Client,
	requestUrl string,
	downloadFilename string,
	ifNoneMatchETag string) (int64, string, error) {

	partialFilename := fmt.Sprintf("%s.part", downloadFilename)

	partialETagFilename := fmt.Sprintf("%s.part.etag", downloadFilename)

	file, err := os.OpenFile(partialFilename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 0, "", ContextError(err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return 0, "", ContextError(err)
	}

	// A partial download should have an ETag which is to be sent with the
	// Range request to ensure that the source object is the same as the
	// one that is partially downloaded.
	var partialETag []byte
	if fileInfo.Size() > 0 {

		partialETag, err = ioutil.ReadFile(partialETagFilename)

		// When the ETag can't be loaded, delete the partial download. To keep the
		// code simple, there is no immediate, inline retry here, on the assumption
		// that the controller's upgradeDownloader will shortly call DownloadUpgrade
		// again.
		if err != nil {
			os.Remove(partialFilename)
			os.Remove(partialETagFilename)
			return 0, "", ContextError(
				fmt.Errorf("failed to load partial download ETag: %s", err))
		}
	}

	request, err := http.NewRequest("GET", requestUrl, nil)
	if err != nil {
		return 0, "", ContextError(err)
	}

	request.Header.Add("Range", fmt.Sprintf("bytes=%d-", fileInfo.Size()))

	if partialETag != nil {

		// Note: not using If-Range, since not all host servers support it.
		// Using If-Match means we need to check for status code 412 and reset
		// when the ETag has changed since the last partial download.
		request.Header.Add("If-Match", string(partialETag))

	} else if ifNoneMatchETag != "" {

		// Can't specify both If-Match and If-None-Match. Behavior is undefined.
		// https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.26
		// So for downloaders that store an ETag and wish to use that to prevent
		// redundant downloads, that ETag is sent as If-None-Match in the case
		// where a partial download is not in progress. When a partial download
		// is in progress, the partial ETag is sent as If-Match: either that's
		// a version that was never fully received, or it's no longer current in
		// which case the response will be StatusPreconditionFailed, the partial
		// download will be discarded, and then the next retry will use
		// If-None-Match.

		// Note: in this case, fileInfo.Size() == 0

		request.Header.Add("If-None-Match", ifNoneMatchETag)
	}

	response, err := httpClient.Do(request)

	// The resumeable download may ask for bytes past the resource range
	// since it doesn't store the "completed download" state. In this case,
	// the HTTP server returns 416. Otherwise, we expect 206. We may also
	// receive 412 on ETag mismatch.
	if err == nil &&
		(response.StatusCode != http.StatusPartialContent &&
			response.StatusCode != http.StatusRequestedRangeNotSatisfiable &&
			response.StatusCode != http.StatusPreconditionFailed &&
			response.StatusCode != http.StatusNotModified) {
		response.Body.Close()
		err = fmt.Errorf("unexpected response status code: %d", response.StatusCode)
	}
	if err != nil {
		return 0, "", ContextError(err)
	}
	defer response.Body.Close()

	responseETag := response.Header.Get("ETag")

	if response.StatusCode == http.StatusPreconditionFailed {
		// When the ETag no longer matches, delete the partial download. As above,
		// simply failing and relying on the caller's retry schedule.
		os.Remove(partialFilename)
		os.Remove(partialETagFilename)
		return 0, "", ContextError(errors.New("partial download ETag mismatch"))

	} else if response.StatusCode == http.StatusNotModified {
		// This status code is possible in the "If-None-Match" case. Don't leave
		// any partial download in progress. Caller should check that responseETag
		// matches ifNoneMatchETag.
		os.Remove(partialFilename)
		os.Remove(partialETagFilename)
		return 0, responseETag, nil
	}

	// Not making failure to write ETag file fatal, in case the entire download
	// succeeds in this one request.
	ioutil.WriteFile(partialETagFilename, []byte(responseETag), 0600)

	// A partial download occurs when this copy is interrupted. The io.Copy
	// will fail, leaving a partial download in place (.part and .part.etag).
	n, err := io.Copy(NewSyncFileWriter(file), response.Body)

	// From this point, n bytes are indicated as downloaded, even if there is
	// an error; the caller may use this to report partial download progress.

	if err != nil {
		return n, "", ContextError(err)
	}

	// Ensure the file is flushed to disk. The deferred close
	// will be a noop when this succeeds.
	err = file.Close()
	if err != nil {
		return n, "", ContextError(err)
	}

	// Remove if exists, to enable rename
	os.Remove(downloadFilename)

	err = os.Rename(partialFilename, downloadFilename)
	if err != nil {
		return n, "", ContextError(err)
	}

	os.Remove(partialETagFilename)

	return n, responseETag, nil
}

// IPAddressFromAddr is a helper which extracts an IP address
// from a net.Addr or returns "" if there is no IP address.
func IPAddressFromAddr(addr net.Addr) string {
	ipAddress := ""
	if addr != nil {
		host, _, err := net.SplitHostPort(addr.String())
		if err == nil {
			ipAddress = host
		}
	}
	return ipAddress
}

// HTTPSServer is a wrapper around http.Server which adds the
// ServeTLS function.
type HTTPSServer struct {
	http.Server
}

// ServeTLS is a offers the equivalent interface as http.Serve.
// The http package has both ListenAndServe and ListenAndServeTLS higher-
// level interfaces, but only Serve (not TLS) offers a lower-level interface that
// allows the caller to keep a refererence to the Listener, allowing for external
// shutdown. ListenAndServeTLS also requires the TLS cert and key to be in files
// and we avoid that here.
// tcpKeepAliveListener is used in http.ListenAndServeTLS but not exported,
// so we use a copy from https://golang.org/src/net/http/server.go.
func (server *HTTPSServer) ServeTLS(listener net.Listener) error {
	tlsListener := tls.NewListener(tcpKeepAliveListener{listener.(*net.TCPListener)}, server.TLSConfig)
	return server.Serve(tlsListener)
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}

// ActivityMonitoredConn wraps a net.Conn, adding logic to deal with
// events triggered by I/O activity.
//
// When an inactivity timeout is specified, the net.Conn Read() will
// timeout after the specified period of read inactivity. Optionally,
// ActivityMonitoredConn will also consider the connection active when
// data is written to it.
//
// When a LRUConnsEntry is specified, then the LRU entry is promoted on
// either a successful read or write.
//
type ActivityMonitoredConn struct {
	net.Conn
	inactivityTimeout time.Duration
	activeOnWrite     bool
	startTime         int64
	lastActivityTime  int64
	lruEntry          *LRUConnsEntry
}

func NewActivityMonitoredConn(
	conn net.Conn,
	inactivityTimeout time.Duration,
	activeOnWrite bool,
	lruEntry *LRUConnsEntry) *ActivityMonitoredConn {

	if inactivityTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(inactivityTimeout))
	}

	now := time.Now().UnixNano()

	return &ActivityMonitoredConn{
		Conn:              conn,
		inactivityTimeout: inactivityTimeout,
		activeOnWrite:     activeOnWrite,
		startTime:         now,
		lastActivityTime:  now,
		lruEntry:          lruEntry,
	}
}

// GetStartTime gets the time when the ActivityMonitoredConn was
// initialized.
func (conn *ActivityMonitoredConn) GetStartTime() time.Time {
	return time.Unix(0, conn.startTime)
}

// GetActiveDuration returns the time elapsed between the initialization
// of the ActivityMonitoredConn and the last Read (or Write when
// activeOnWrite is specified).
func (conn *ActivityMonitoredConn) GetActiveDuration() time.Duration {
	return time.Duration(atomic.LoadInt64(&conn.lastActivityTime) - conn.startTime)
}

func (conn *ActivityMonitoredConn) Read(buffer []byte) (int, error) {
	n, err := conn.Conn.Read(buffer)
	if err == nil {

		atomic.StoreInt64(&conn.lastActivityTime, time.Now().UnixNano())

		if conn.inactivityTimeout > 0 {
			conn.Conn.SetReadDeadline(time.Now().Add(conn.inactivityTimeout))
		}

		if conn.lruEntry != nil {
			conn.lruEntry.Touch()
		}
	}
	return n, err
}

func (conn *ActivityMonitoredConn) Write(buffer []byte) (int, error) {
	n, err := conn.Conn.Write(buffer)
	if err == nil {

		if conn.activeOnWrite {

			atomic.StoreInt64(&conn.lastActivityTime, time.Now().UnixNano())

			if conn.inactivityTimeout > 0 {
				conn.Conn.SetReadDeadline(time.Now().Add(conn.inactivityTimeout))
			}
		}

		if conn.lruEntry != nil {
			conn.lruEntry.Touch()
		}
	}
	return n, err
}

// ThrottledConn wraps a net.Conn with read and write rate limiters.
// Rates are specified as bytes per second. Optional unlimited byte
// counts allow for a number of bytes to read or write before
// applying rate limiting. Specify limit values of 0 to set no rate
// limit (unlimited counts are ignored in this case).
// The underlying rate limiter uses the token bucket algorithm to
// calculate delay times for read and write operations.
type ThrottledConn struct {
	net.Conn
	unlimitedReadBytes  int64
	limitingReads       int32
	limitedReader       io.Reader
	unlimitedWriteBytes int64
	limitingWrites      int32
	limitedWriter       io.Writer
}

// NewThrottledConn initializes a new ThrottledConn.
func NewThrottledConn(
	conn net.Conn,
	unlimitedReadBytes, limitReadBytesPerSecond,
	unlimitedWriteBytes, limitWriteBytesPerSecond int64) *ThrottledConn {

	// When no limit is specified, the rate limited reader/writer
	// is simply the base reader/writer.

	var reader io.Reader
	if limitReadBytesPerSecond == 0 {
		reader = conn
	} else {
		reader = ratelimit.Reader(conn,
			ratelimit.NewBucketWithRate(
				float64(limitReadBytesPerSecond), limitReadBytesPerSecond))
	}

	var writer io.Writer
	if limitWriteBytesPerSecond == 0 {
		writer = conn
	} else {
		writer = ratelimit.Writer(conn,
			ratelimit.NewBucketWithRate(
				float64(limitWriteBytesPerSecond), limitWriteBytesPerSecond))
	}

	return &ThrottledConn{
		Conn:                conn,
		unlimitedReadBytes:  unlimitedReadBytes,
		limitingReads:       0,
		limitedReader:       reader,
		unlimitedWriteBytes: unlimitedWriteBytes,
		limitingWrites:      0,
		limitedWriter:       writer,
	}
}

func (conn *ThrottledConn) Read(buffer []byte) (int, error) {

	// Use the base reader until the unlimited count is exhausted.
	if atomic.LoadInt32(&conn.limitingReads) == 0 {
		if atomic.AddInt64(&conn.unlimitedReadBytes, -int64(len(buffer))) <= 0 {
			atomic.StoreInt32(&conn.limitingReads, 1)
		} else {
			return conn.Read(buffer)
		}
	}

	return conn.limitedReader.Read(buffer)
}

func (conn *ThrottledConn) Write(buffer []byte) (int, error) {

	// Use the base writer until the unlimited count is exhausted.
	if atomic.LoadInt32(&conn.limitingWrites) == 0 {
		if atomic.AddInt64(&conn.unlimitedWriteBytes, -int64(len(buffer))) <= 0 {
			atomic.StoreInt32(&conn.limitingWrites, 1)
		} else {
			return conn.Write(buffer)
		}
	}

	return conn.limitedWriter.Write(buffer)
}
