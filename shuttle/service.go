package main

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/litl/galaxy/log"
	"github.com/litl/galaxy/shuttle/client"
)

var (
	Registry = ServiceRegistry{
		svcs:   make(map[string]*Service),
		vhosts: make(map[string]*VirtualHost),
	}
)

type Service struct {
	sync.Mutex
	Name          string
	Addr          string
	VirtualHosts  []string
	Backends      []*Backend
	Balance       string
	CheckInterval int
	Fall          int
	Rise          int
	ClientTimeout time.Duration
	ServerTimeout time.Duration
	DialTimeout   time.Duration
	Sent          int64
	Rcvd          int64
	Errors        int64
	HTTPConns     int64
	HTTPErrors    int64
	HTTPActive    int64

	// Next returns the backends in priority order.
	next func() []*Backend

	// the last backend we used and the number of times we used it
	lastBackend int
	lastCount   int

	// Each Service owns it's own netowrk listener
	listener net.Listener

	// reverse proxy for vhost routing
	httpProxy *ReverseProxy

	// Custom Pages to backend error responses
	errorPages *ErrorResponse

	// the original map of errors as loaded in by a config
	errPagesCfg map[string][]int

	// net.Dialer so we don't need to allocate one every time
	dialer *net.Dialer
}

// Stats returned about a service
type ServiceStat struct {
	Name          string        `json:"name"`
	Addr          string        `json:"address"`
	VirtualHosts  []string      `json:"virtual_hosts"`
	Backends      []BackendStat `json:"backends"`
	Balance       string        `json:"balance"`
	CheckInterval int           `json:"check_interval"`
	Fall          int           `json:"fall"`
	Rise          int           `json:"rise"`
	ClientTimeout int           `json:"client_timeout"`
	ServerTimeout int           `json:"server_timeout"`
	DialTimeout   int           `json:"connect_timeout"`
	Sent          int64         `json:"sent"`
	Rcvd          int64         `json:"received"`
	Errors        int64         `json:"errors"`
	Conns         int64         `json:"connections"`
	Active        int64         `json:"active"`
	HTTPActive    int64         `json:"http_active"`
	HTTPConns     int64         `json:"http_connections"`
	HTTPErrors    int64         `json:"http_errors"`
}

// Create a Service from a config struct
func NewService(cfg client.ServiceConfig) *Service {
	s := &Service{
		Name:          cfg.Name,
		Addr:          cfg.Addr,
		CheckInterval: cfg.CheckInterval,
		Fall:          cfg.Fall,
		Rise:          cfg.Rise,
		VirtualHosts:  cfg.VirtualHosts,
		ClientTimeout: time.Duration(cfg.ClientTimeout) * time.Millisecond,
		ServerTimeout: time.Duration(cfg.ServerTimeout) * time.Millisecond,
		DialTimeout:   time.Duration(cfg.DialTimeout) * time.Millisecond,
		errorPages:    NewErrorResponse(cfg.ErrorPages),
		errPagesCfg:   cfg.ErrorPages,
	}

	// TODO: insert this into the backends too
	s.dialer = &net.Dialer{
		Timeout:   s.DialTimeout,
		KeepAlive: 30 * time.Second,
	}

	// create our reverse proxy, using our load-balancing Dial method
	proxyTransport := &http.Transport{
		Dial:                s.Dial,
		MaxIdleConnsPerHost: 10,
	}
	s.httpProxy = NewReverseProxy(proxyTransport)
	s.httpProxy.FlushInterval = time.Second
	s.httpProxy.Director = func(req *http.Request) {
		req.URL.Scheme = "http"
	}

	s.httpProxy.OnRequest = []ProxyCallback{sslRedirect}
	s.httpProxy.OnResponse = []ProxyCallback{logProxyRequest, s.errStats, s.errorPages.CheckResponse}

	if s.CheckInterval == 0 {
		s.CheckInterval = 2000
	}
	if s.Rise == 0 {
		s.Rise = 2
	}
	if s.Fall == 0 {
		s.Fall = 2
	}

	for _, b := range cfg.Backends {
		s.add(NewBackend(b))
	}

	switch cfg.Balance {
	case "RR", "":
		s.next = s.roundRobin
	case "LC":
		s.next = s.leastConn
	default:
		log.Printf("invalid balancing algorithm '%s'", cfg.Balance)
	}

	return s
}

func (s *Service) Stats() ServiceStat {
	s.Lock()
	defer s.Unlock()

	stats := ServiceStat{
		Name:          s.Name,
		Addr:          s.Addr,
		VirtualHosts:  s.VirtualHosts,
		Balance:       s.Balance,
		CheckInterval: s.CheckInterval,
		Fall:          s.Fall,
		Rise:          s.Rise,
		ClientTimeout: int(s.ClientTimeout / time.Millisecond),
		ServerTimeout: int(s.ServerTimeout / time.Millisecond),
		DialTimeout:   int(s.DialTimeout / time.Millisecond),
		HTTPConns:     s.HTTPConns,
		HTTPErrors:    s.HTTPErrors,
		HTTPActive:    atomic.LoadInt64(&s.HTTPActive),
	}

	for _, b := range s.Backends {
		stats.Backends = append(stats.Backends, b.Stats())
		stats.Sent += b.Sent
		stats.Rcvd += b.Rcvd
		stats.Errors += b.Errors
		stats.Conns += b.Conns
		stats.Active += b.Active
	}

	return stats
}

func (s *Service) Config() client.ServiceConfig {
	s.Lock()
	defer s.Unlock()

	config := client.ServiceConfig{
		Name:          s.Name,
		Addr:          s.Addr,
		VirtualHosts:  s.VirtualHosts,
		Balance:       s.Balance,
		CheckInterval: s.CheckInterval,
		Fall:          s.Fall,
		Rise:          s.Rise,
		ClientTimeout: int(s.ClientTimeout / time.Millisecond),
		ServerTimeout: int(s.ServerTimeout / time.Millisecond),
		DialTimeout:   int(s.DialTimeout / time.Millisecond),
		ErrorPages:    s.errPagesCfg,
	}
	for _, b := range s.Backends {
		config.Backends = append(config.Backends, b.Config())
	}

	return config
}

func (s *Service) String() string {
	return string(marshal(s.Config()))
}

func (s *Service) get(name string) *Backend {
	s.Lock()
	defer s.Unlock()

	for _, b := range s.Backends {
		if b.Name == name {
			return b
		}
	}
	return nil
}

// Add or replace a Backend in this service
func (s *Service) add(backend *Backend) {
	s.Lock()
	defer s.Unlock()

	log.Printf("Adding TCP backend %s for %s at %s", backend.Addr, s.Name, s.Addr)
	backend.up = true
	backend.rwTimeout = s.ServerTimeout
	backend.dialTimeout = s.DialTimeout
	backend.checkInterval = time.Duration(s.CheckInterval) * time.Millisecond

	// replace an existing backend if we have it.
	for i, b := range s.Backends {
		if b.Name == backend.Name {
			b.Stop()
			s.Backends[i] = backend
			backend.Start()
			return
		}
	}

	s.Backends = append(s.Backends, backend)

	backend.Start()
}

// Remove a Backend by name
func (s *Service) remove(name string) bool {
	s.Lock()
	defer s.Unlock()

	for i, b := range s.Backends {
		if b.Name == name {
			log.Printf("Removing TCP backend %s for %s at %s", b.Addr, s.Name, s.Addr)
			last := len(s.Backends) - 1
			deleted := b
			s.Backends[i], s.Backends[last] = s.Backends[last], nil
			s.Backends = s.Backends[:last]
			deleted.Stop()
			return true
		}
	}
	return false
}

// Fill out and verify service
func (s *Service) start() (err error) {
	s.Lock()
	defer s.Unlock()
	log.Printf("Starting TCP listener for %s on %s", s.Name, s.Addr)

	s.listener, err = newTimeoutListener(s.Addr, s.ClientTimeout)
	if err != nil {
		return err
	}

	if s.Backends == nil {
		s.Backends = make([]*Backend, 0)
	}

	s.run()
	return nil
}

// Start the Service's Accept loop
func (s *Service) run() {
	go func() {
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				if err, ok := err.(*net.OpError); ok && err.Temporary() {
					log.Warnln("WARN:", err)
					continue
				}
				// we must be getting shut down
				return
			}

			go s.connect(conn)
		}
	}()
}

// Return the addresses of the current backends in the order they would be balanced
func (s *Service) NextAddrs() []string {
	backends := s.next()

	addrs := make([]string, len(backends))
	for i, b := range backends {
		addrs[i] = b.Addr
	}
	return addrs
}

// Available returns the number of backends marked as Up
func (s *Service) Available() int {
	s.Lock()
	defer s.Unlock()

	available := 0
	for _, b := range s.Backends {
		if b.Up() {
			available++
		}
	}
	return available
}

// Dial a backend by address.
// This way we can wrap the connection to provide our timeout settings, as well
// as hook it into the backend stats.
// We return an error if we don't have a backend which matches.
// If Dial returns an error, we wrap it in DialError, so that a ReverseProxy
// can determine if it's safe to call RoundTrip again on a new host.
func (s *Service) Dial(nw, addr string) (net.Conn, error) {
	s.Lock()

	var backend *Backend
	for _, b := range s.Backends {
		if b.Addr == addr {
			backend = b
			break
		}
	}
	s.Unlock()

	if backend == nil {
		return nil, DialError{fmt.Errorf("no backend matching %s", addr)}
	}

	srvConn, err := s.dialer.Dial("tcp", backend.Addr)
	if err != nil {
		log.Errorf("ERROR: connecting to backend %s/%s: %s", s.Name, backend.Name, err)
		atomic.AddInt64(&backend.Errors, 1)
		return nil, DialError{err}
	}

	conn := NewConn(
		srvConn.(*net.TCPConn),
		s.ServerTimeout,
		&backend.Sent,
		&backend.Rcvd,
		&backend.HTTPActive,
	)

	atomic.AddInt64(&backend.Conns, 1)

	// NOTE: this relies on conn.Close being called, which *should* happen in
	// all cases, but may be at fault in the active count becomes skewed in
	// some error case.
	atomic.AddInt64(&backend.HTTPActive, 1)
	return conn, nil
}

func (s *Service) connect(cliConn net.Conn) {
	backends := s.next()

	// Try the first backend given, but if that fails, cycle through them all
	// to make a best effort to connect the client.
	for _, b := range backends {
		srvConn, err := s.dialer.Dial("tcp", b.Addr)
		if err != nil {
			log.Errorf("ERROR: connecting to backend %s/%s: %s", s.Name, b.Name, err)
			atomic.AddInt64(&b.Errors, 1)
			continue
		}

		b.Proxy(srvConn, cliConn)
		return
	}

	log.Errorf("ERROR: no backend for %s", s.Name)
	cliConn.Close()
}

// Stop the Service's Accept loop by closing the Listener,
// and stop all backends for this service.
func (s *Service) stop() {
	s.Lock()
	defer s.Unlock()

	log.Printf("Stopping TCP listener for %s on %s", s.Name, s.Addr)
	for _, backend := range s.Backends {
		backend.Stop()
	}

	// the service may have been bad, and the listener failed
	if s.listener == nil {
		return
	}

	err := s.listener.Close()
	if err != nil {
		log.Println(err)
	}
}

// Provide a ServeHTTP method for out ReverseProxy
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&s.HTTPConns, 1)
	atomic.AddInt64(&s.HTTPActive, 1)
	defer atomic.AddInt64(&s.HTTPActive, -1)

	s.httpProxy.ServeHTTP(w, r, s.NextAddrs())
}

func (s *Service) errStats(pr *ProxyRequest) bool {
	if pr.ProxyError != nil {
		atomic.AddInt64(&s.HTTPErrors, 1)
	}
	return true
}

// A net.Listener that provides a read/write timeout
type timeoutListener struct {
	net.Listener
	rwTimeout time.Duration

	// these aren't reported yet, but our new counting connections need to
	// update something
	read    int64
	written int64
}

func newTimeoutListener(addr string, timeout time.Duration) (net.Listener, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	tl := &timeoutListener{
		Listener:  l,
		rwTimeout: timeout,
	}
	return tl, nil
}
func (l *timeoutListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	c, ok := conn.(*net.TCPConn)
	if ok {
		tc := NewConn(
			c,
			l.rwTimeout,
			&l.read,
			&l.written,
			nil,
		)
		return tc, nil
	}
	return conn, nil
}
