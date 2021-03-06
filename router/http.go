package main

import (
	"bufio"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/kavu/go_reuseport"
	"github.com/flynn/flynn/Godeps/_workspace/src/golang.org/x/crypto/nacl/secretbox"
	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/httphelper"
	"github.com/flynn/flynn/pkg/random"
	"github.com/flynn/flynn/pkg/tlsconfig"
	"github.com/flynn/flynn/router/types"
)

type HTTPListener struct {
	Watcher
	DataStoreReader

	Addr    string
	TLSAddr string

	mtx      sync.RWMutex
	domains  map[string]*httpRoute
	routes   map[string]*httpRoute
	services map[string]*httpService

	discoverd DiscoverdClient
	ds        DataStore
	wm        *WatchManager

	listener    net.Listener
	tlsListener net.Listener
	closed      bool
	cookieKey   *[32]byte
	keypair     tls.Certificate
}

type DiscoverdClient interface {
	Service(string) discoverd.Service
}

func (s *HTTPListener) Close() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	for _, service := range s.services {
		service.sc.Close()
	}
	s.listener.Close()
	s.tlsListener.Close()
	s.ds.StopSync()
	s.closed = true
	return nil
}

func (s *HTTPListener) Start() error {
	if s.Watcher != nil {
		return errors.New("router: http listener already started")
	}
	if s.wm == nil {
		s.wm = NewWatchManager()
	}
	s.Watcher = s.wm

	if s.ds == nil {
		return errors.New("router: http listener missing data store")
	}
	s.DataStoreReader = s.ds

	s.routes = make(map[string]*httpRoute)
	s.domains = make(map[string]*httpRoute)
	s.services = make(map[string]*httpService)

	if s.cookieKey == nil {
		s.cookieKey = &[32]byte{}
	}

	started := make(chan error)

	go s.ds.Sync(&httpSyncHandler{l: s}, started)
	if err := <-started; err != nil {
		return err
	}

	go s.listenAndServe(started)
	if err := <-started; err != nil {
		s.ds.StopSync()
		return err
	}
	s.Addr = s.listener.Addr().String()

	go s.listenAndServeTLS(started)
	if err := <-started; err != nil {
		s.ds.StopSync()
		s.listener.Close()
		return err
	}
	s.TLSAddr = s.tlsListener.Addr().String()

	return nil
}

var ErrClosed = errors.New("router: listener has been closed")

func (s *HTTPListener) AddRoute(r *router.Route) error {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	if s.closed {
		return ErrClosed
	}
	r.ID = md5sum(r.HTTPRoute().Domain)
	return s.ds.Add(r)
}

func (s *HTTPListener) SetRoute(r *router.Route) error {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	if s.closed {
		return ErrClosed
	}
	r.ID = md5sum(r.HTTPRoute().Domain)
	return s.ds.Set(r)
}

func md5sum(data string) string {
	digest := md5.Sum([]byte(data))
	return hex.EncodeToString(digest[:])
}

func (s *HTTPListener) RemoveRoute(id string) error {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	if s.closed {
		return ErrClosed
	}
	return s.ds.Remove(id)
}

type httpSyncHandler struct {
	l *HTTPListener
}

func (h *httpSyncHandler) Set(data *router.Route) error {
	route := data.HTTPRoute()
	r := &httpRoute{HTTPRoute: route}

	if r.TLSCert != "" && r.TLSKey != "" {
		kp, err := tls.X509KeyPair([]byte(r.TLSCert), []byte(r.TLSKey))
		if err != nil {
			return err
		}
		r.keypair = &kp
		r.TLSCert = ""
		r.TLSKey = ""
	}

	h.l.mtx.Lock()
	defer h.l.mtx.Unlock()
	if h.l.closed {
		return nil
	}

	service := h.l.services[r.Service]
	if service != nil && service.name != r.Service {
		service.refs--
		if service.refs <= 0 {
			service.sc.Close()
			delete(h.l.services, service.name)
		}
		service = nil
	}
	if service == nil {
		sc, err := NewDiscoverdServiceCache(h.l.discoverd.Service(r.Service))
		if err != nil {
			return err
		}
		service = &httpService{name: r.Service, sc: sc, cookieKey: h.l.cookieKey}
		h.l.services[r.Service] = service
	}
	service.refs++
	r.service = service
	h.l.routes[data.ID] = r
	h.l.domains[strings.ToLower(r.Domain)] = r

	go h.l.wm.Send(&router.Event{Event: "set", ID: r.Domain})
	return nil
}

func (h *httpSyncHandler) Remove(id string) error {
	h.l.mtx.Lock()
	defer h.l.mtx.Unlock()
	if h.l.closed {
		return nil
	}
	r, ok := h.l.routes[id]
	if !ok {
		return ErrNotFound
	}

	r.service.refs--
	if r.service.refs <= 0 {
		r.service.sc.Close()
		delete(h.l.services, r.service.name)
	}

	delete(h.l.routes, id)
	delete(h.l.domains, r.Domain)
	go h.l.wm.Send(&router.Event{Event: "remove", ID: id})
	return nil
}

func (s *HTTPListener) listenAndServe(started chan<- error) {
	var err error
	s.listener, err = reuseport.NewReusablePortListener("tcp4", s.Addr)
	started <- err
	if err != nil {
		return
	}
	server := &http.Server{
		Addr: s.listener.Addr().String(),
		Handler: fwdProtoHandler{
			Handler: s,
			Proto:   "http",
			Port:    mustPortFromAddr(s.listener.Addr().String()),
		},
	}

	// TODO: log error
	_ = server.Serve(s.listener)
}

var errMissingTLS = errors.New("router: route not found or TLS not configured")

func (s *HTTPListener) listenAndServeTLS(started chan<- error) {
	certForHandshake := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		r := s.findRouteForHost(hello.ServerName)
		if r == nil {
			return nil, errMissingTLS
		}
		return r.keypair, nil
	}
	tlsConfig := tlsconfig.SecureCiphers(&tls.Config{
		GetCertificate: certForHandshake,
		Certificates:   []tls.Certificate{s.keypair},
	})

	l, err := reuseport.NewReusablePortListener("tcp4", s.TLSAddr)
	if err == nil {
		s.tlsListener = tls.NewListener(l, tlsConfig)
	}
	started <- err
	if err != nil {
		return
	}

	server := &http.Server{
		Addr: s.tlsListener.Addr().String(),
		Handler: fwdProtoHandler{
			Handler: s,
			Proto:   "https",
			Port:    mustPortFromAddr(s.tlsListener.Addr().String()),
		},
	}

	// TODO: log error
	_ = server.Serve(s.tlsListener)
}

func (s *HTTPListener) findRouteForHost(host string) *httpRoute {
	host = strings.ToLower(host)
	if strings.Contains(host, ":") {
		host, _, _ = net.SplitHostPort(host)
	}
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	if backend, ok := s.domains[host]; ok {
		return backend
	}
	// handle wildcard domains up to 5 subdomains deep, from most-specific to
	// least-specific
	d := strings.SplitN(host, ".", 5)
	for i := len(d); i > 0; i-- {
		if backend, ok := s.domains["*."+strings.Join(d[len(d)-i:], ".")]; ok {
			return backend
		}
	}
	return nil
}

func failAndClose(w http.ResponseWriter, code int) {
	w.Header().Set("Connection", "close")
	fail(w, code)
}

func fail(w http.ResponseWriter, code int) {
	msg := []byte(http.StatusText(code) + "\n")
	w.Header().Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(code)
	w.Write(msg)
}

const hdrUseStickySessions = "Flynn-Use-Sticky-Sessions"

func (s *HTTPListener) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r := s.findRouteForHost(req.Host)
	if r == nil {
		fail(w, 404)
		return
	}

	// TODO(bgentry): find a better way to access this setting in the service
	// where it's needed.
	stickyValue := "false"
	if r.Sticky {
		stickyValue = "true"
	}
	req.Header.Set(hdrUseStickySessions, stickyValue)

	r.service.ServeHTTP(w, req)
}

// A domain served by a listener, associated TLS certs,
// and link to backend service set.
type httpRoute struct {
	*router.HTTPRoute

	keypair *tls.Certificate
	service *httpService
}

// A service definition: name, and set of backends.
type httpService struct {
	name string
	sc   DiscoverdServiceCache
	refs int

	cookieKey *[32]byte
}

const stickyCookie = "_backend"

func (s *httpService) stickyCookieAddr(req *http.Request) string {
	cookie, err := req.Cookie(stickyCookie)
	if err != nil {
		return ""
	}

	data, err := base64.StdEncoding.DecodeString(cookie.Value)
	if err != nil {
		return ""
	}
	var nonce [24]byte
	if len(data) < len(nonce) {
		return ""
	}
	copy(nonce[:], data)
	res, ok := secretbox.Open(nil, data[len(nonce):], &nonce, s.cookieKey)
	if !ok {
		return ""
	}

	addr := string(res)
	ok = false
	for _, a := range s.sc.Addrs() {
		if a == addr {
			ok = true
			break
		}
	}
	if !ok {
		return ""
	}

	return addr
}

func (s *httpService) newStickyCookie(backend string) *http.Cookie {
	var nonce [24]byte
	_, err := io.ReadFull(rand.Reader, nonce[:])
	if err != nil {
		panic(err)
	}
	out := make([]byte, len(nonce), len(nonce)+len(backend)+secretbox.Overhead)
	copy(out, nonce[:])
	out = secretbox.Seal(out, []byte(backend), &nonce, s.cookieKey)

	return &http.Cookie{Name: stickyCookie, Value: base64.StdEncoding.EncodeToString(out), Path: "/"}
}

func (s *httpService) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	req.Header.Set("X-Request-Start", strconv.FormatInt(time.Now().UnixNano()/int64(time.Millisecond), 10))
	req.Header.Set("X-Request-Id", random.UUID())

	addrs := shuffle(s.sc.Addrs())
	if len(addrs) == 0 {
		log.Println("no backends found")
		fail(w, 503)
		return
	}

	isSticky := false
	stickyAddr := ""
	if req.Header.Get(hdrUseStickySessions) == "true" {
		// TODO(bgentry): switch to better way to check sticky setting
		isSticky = true
		if stickyAddr = s.stickyCookieAddr(req); stickyAddr != "" {
			sortStringFirst(addrs, stickyAddr)
		}
	}
	req.Header.Del(hdrUseStickySessions) // delete this no matter what

	// A proxy or gateway MUST parse a received Connection header field before a
	// message is forwarded: https://tools.ietf.org/html/rfc7230#section-6.1
	reqConnHdrOpts := parseConnHeader(req.Header.Get("Connection"))
	isRequestConnUpgrade := isConnUpgrade(reqConnHdrOpts)

	// Most of this is borrowed from httputil.ReverseProxy
	outreq := &http.Request{}
	*outreq = *req // includes shallow copies of maps, but okay

	// Pass the Request-URI verbatim without any modifications
	outreq.URL.Opaque = strings.Split(strings.TrimPrefix(req.RequestURI, req.URL.Scheme+":"), "?")[0]
	outreq.URL.Scheme = "http"
	outreq.Proto = "HTTP/1.1"
	outreq.ProtoMajor = 1
	outreq.ProtoMinor = 1
	outreq.Close = false

	// TODO: Proxy HTTP CONNECT? (example: Go RPC over HTTP)

	// Remove hop-by-hop headers to the backend.  This is modifying the same
	// underlying map from req (shallow copied above) so we only copy it if
	// necessary.
	outreq.Header = make(http.Header, len(req.Header))
	copyHeader(outreq.Header, req.Header)
	for _, h := range alwaysHopHeaders {
		outreq.Header.Del(h)
	}

	// remove the Upgrade header if HTTP < 1.1 or if Connection header didn't
	// contain "upgrade": https://tools.ietf.org/html/rfc7230#section-6.7
	if outreq.Header.Get("Upgrade") != "" && (!req.ProtoAtLeast(1, 1) || !isRequestConnUpgrade) {
		outreq.Header.Del("Upgrade")
	}

	// Directly bridge `Connection: Upgrade` requests
	if isRequestConnUpgrade {
		s.forwardAndProxyTCP(w, outreq, addrs, stickyAddr, isSticky)
		return
	}

	// A proxy or gateway MUST parse a received Connection header field before a
	// message is forwarded and, for each connection-option in this field, remove
	// any header field(s) from the message with the same name as the
	// connection-option, and then remove the Connection header field itself (or
	// replace it with the intermediary's own connection options for the
	// forwarded message): https://tools.ietf.org/html/rfc7230#section-6.1
	outreq.Header.Del("Connection")
	for _, opt := range reqConnHdrOpts {
		outreq.Header.Del(opt)
	}

	var (
		backend string
		res     *http.Response
		err     error
	)

	for _, backend = range addrs {
		// TODO: limit number of backends tried
		// TODO: temporarily quarantine failing backends

		outreq.URL.Host = backend
		res, err = transport.RoundTrip(outreq)
		if err != nil {
			if _, ok := err.(dialErr); ok {
				// retry, maybe log a message about it
				continue
			}
			log.Println("http: proxy error:", err)
			fail(w, 503)
			return
		}
		defer res.Body.Close()
		break
	}

	if res == nil {
		log.Println("no backends available")
		fail(w, 503)
		return
	}

	if isSticky && stickyAddr != backend {
		http.SetCookie(w, s.newStickyCookie(backend))
	}

	// A proxy or gateway MUST parse a received Connection header field before a
	// message is forwarded: https://tools.ietf.org/html/rfc7230#section-6.1
	respConnHdrOpts := parseConnHeader(res.Header.Get("Connection"))
	res.Header.Del("Connection")
	for _, h := range alwaysHopHeaders {
		res.Header.Del(h)
	}
	// remove hop-by-hop headers as specified in connection options
	for _, opt := range respConnHdrOpts {
		res.Header.Del(opt)
	}
	// remove the Upgrade header if HTTP < 1.1 or if Connection header didn't
	// contain "upgrade": https://tools.ietf.org/html/rfc7230#section-6.7
	if res.Header.Get("Upgrade") != "" && (!req.ProtoAtLeast(1, 1) || !isConnUpgrade(respConnHdrOpts)) {
		res.Header.Del("Upgrade")
	}

	copyHeader(w.Header(), res.Header)

	w.WriteHeader(res.StatusCode)
	w.(http.Flusher).Flush()
	_, err = io.Copy(httphelper.FlushWriter{Writer: w, Enabled: true}, res.Body) // TODO(bgentry): consider using a flush interval
	if err != nil {
		log.Println("reverse proxy copy err:", err)
		return
	}
}

func sortStringFirst(ss []string, s string) {
	for i := range ss {
		if ss[i] == s {
			ss[0], ss[i] = ss[i], ss[0]
		}
	}
}

var transport http.RoundTripper = &http.Transport{
	Dial:                customDial,
	TLSHandshakeTimeout: 10 * time.Second, // unused, but safer to leave default in place
}

var dialer = &net.Dialer{
	Timeout:   1 * time.Second,
	KeepAlive: 30 * time.Second,
}

func customDial(network, addr string) (net.Conn, error) {
	conn, err := dialer.Dial(network, addr)
	if err != nil {
		return nil, dialErr{err}
	}
	return conn, nil
}

type dialErr struct {
	error
}

func (s *httpService) forwardAndProxyTCP(w http.ResponseWriter, req *http.Request, addrs []string, stickyAddr string, isSticky bool) {
	var (
		backend string
		err     error
		upconn  net.Conn
	)
	for _, backend = range addrs {
		req.URL.Host = backend
		upconn, err = dialer.Dial("tcp", req.URL.Host)
		if err != nil {
			// retry, maybe log a message about it
			continue
		}
		defer upconn.Close()
		break
	}
	if upconn == nil {
		log.Println("no backends available")
		failAndClose(w, 503)
		return
	}

	err = req.Write(upconn)
	if err != nil {
		log.Println("error copying request to target:", err)
		failAndClose(w, 503)
		return
	}

	// Need to complete the handshake and set sticky cookie on response, otherwise
	// websocket reconnections won't go to the right backend
	upconnbr := bufio.NewReader(upconn)
	res, err := http.ReadResponse(upconnbr, req)
	if err != nil {
		log.Println("http: proxy error:", err)
		failAndClose(w, 503)
		return
	}
	defer res.Body.Close()

	respConnHdrOpts := parseConnHeader(res.Header.Get("Connection"))
	for _, h := range alwaysHopHeaders {
		res.Header.Del(h)
	}
	if res.Header.Get("Upgrade") != "" && !isConnUpgrade(respConnHdrOpts) {
		res.Header.Del("Upgrade")
	}

	// Copy the response headers and body over to the downstream ResponseWriter,
	// the same as done in the non-TCP path.
	copyHeader(w.Header(), res.Header)

	if isSticky && stickyAddr != backend {
		http.SetCookie(w, s.newStickyCookie(backend))
	}

	w.WriteHeader(res.StatusCode)
	_, err = io.Copy(w, res.Body) // TODO(bgentry): consider using a flush interval
	if err != nil {
		log.Println("reverse proxy copy err:", err)
		return
	}

	if res.StatusCode != 101 {
		return
	}
	res.Body.Close() // close this now since we've copied everything

	downconn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Println("hijack failed:", err)
		failAndClose(w, 500)
		return
	}
	defer downconn.Close()

	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		errc <- err
	}
	go cp(upconn, downconn)
	go cp(downconn, upconnbr)
	<-errc
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isConnUpgrade(connOpts []string) bool {
	for _, opt := range connOpts {
		if opt == "upgrade" {
			return true
		}
	}
	return false
}

// Hop-by-hop headers. These are removed when sent to the backend (or forwarded
// to the client) because they only apply to a single connection.
var alwaysHopHeaders = []string{
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
}

func parseConnHeader(value string) []string {
	splitOpts := strings.Split(value, ",")
	headerOpts := make([]string, 0, len(splitOpts))
	for _, opt := range splitOpts {
		// remove empty values, trim space
		if opt = strings.ToLower(strings.TrimSpace(opt)); opt != "" {
			headerOpts = append(headerOpts, opt)
		}
	}
	return headerOpts
}

func mustPortFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	return port
}

type writeCloser interface {
	CloseWrite() error
}

func shuffle(s []string) []string {
	for i := len(s) - 1; i > 0; i-- {
		j := random.Math.Intn(i + 1)
		s[i], s[j] = s[j], s[i]
	}
	return s
}
