package zerver

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cosiner/gohper/lib/defval"
	"github.com/cosiner/gohper/lib/errors"
	"github.com/cosiner/gohper/lib/types"
	websocket "github.com/cosiner/zerver_websocket"
)

const (
	ErrComponentNotFound = errors.Err("The required component is not found")
	// server status
	_NORMAL    = 0
	_DESTROYED = 1

	_CONTENTTYPE_DISABLE = "-"
)

var (
	Bytes  = types.UnsafeBytes
	String = types.UnsafeString
)

type (
	ServerOption struct {
		// server listening address, default :4000
		ListenAddr string
		// content type for each request, default application/json;charset=utf-8,
		// use "-" to disable the automation
		ContentType string

		// check websocket header, default nil
		WebSocketChecker HeaderChecker
		// logger, default use cosiner/gohper/log.Logger with ConsoleWriter
		Logger

		// path variables count, suggest set as max or average, default 3
		PathVarCount int
		// filters count for each route, RootFilters is not include, default 5
		FilterCount int

		// read timeout by millseconds
		ReadTimeout int
		// write timeout by millseconds
		WriteTimeout int
		// max header bytes
		MaxHeaderBytes int
		// tcp keep-alive period by minutes,
		// default 3, same as predefined in standard http package
		KeepAlivePeriod int
		// ssl config, default disable tls
		CertFile, KeyFile string
		// if not nil, cert and key will be ignored
		TLSConfig *tls.Config
	}

	// Server represent a web server
	Server struct {
		Router
		AttrContainer
		RootFilters    RootFilters // Match Every Routes
		ResourceMaster ResourceMaster
		Log            Logger

		components        map[string]ComponentState
		managedComponents []Component
		sync.RWMutex

		checker     websocket.HandshakeChecker
		contentType string // default content type

		listener    net.Listener
		state       int32          // destroy or normal running
		activeConns sync.WaitGroup // connections in service, don't include hijacked and websocket connections
	}

	// Component is a Object which will automaticlly initial/destroyed by server
	// if it's added to server, else it should initial manually
	Component interface {
		Init(Enviroment) error
		Destroy()
	}

	ComponentState struct {
		Initialized bool
		NoLazy      bool
		Component
	}

	// HeaderChecker is a http header checker, it accept a function which can get
	// http header's value by name , if there is something wrong, throw an error
	// to terminate this request
	HeaderChecker func(func(string) string) error

	// Enviroment is a server enviroment, real implementation is the Server itself.
	// it can be accessed from Request/WebsocketConn
	Enviroment interface {
		Server() *Server
		Logger() Logger
		StartTask(path string, value interface{})
		Component(name string) (Component, error)
	}
)

func (o *ServerOption) init() {
	defval.String(&o.ListenAddr, ":4000")
	defval.String(&o.ContentType, CONTENTTYPE_JSON)
	defval.Int(&o.PathVarCount, 3)
	defval.Int(&o.FilterCount, 5)
	defval.Int(&o.KeepAlivePeriod, 3) // same as net/http/server.go:tcpKeepAliveListener

	if o.Logger == nil {
		o.Logger = DefaultLogger()
	}
}

// NewServer create a new server with default router
func NewServer() *Server {
	return NewServerWith(nil, nil)
}

// NewServerWith create a new server with given router and root filters
func NewServerWith(rt Router, filters RootFilters) *Server {
	if filters == nil {
		filters = NewRootFilters(nil)
	}
	if rt == nil {
		rt = NewRouter()
	}
	return &Server{
		Router:         rt,
		AttrContainer:  NewLockedAttrContainer(),
		RootFilters:    filters,
		components:     make(map[string]ComponentState),
		ResourceMaster: newResourceMaster(),
	}
}

// ent ServerEnviroment
func (s *Server) Server() *Server {
	return s
}

func (s *Server) Logger() Logger {
	return s.Log
}

func (s *Server) Component(name string) (Component, error) {
	s.RLock()
	if c, has := s.components[name]; has {
		s.RUnlock()
		var err error
		if !c.Initialized {
			s.Lock()
			if !c.Initialized {
				if err = c.Component.Init(s); err == nil {
					c.Initialized = true
				}
			}
			s.Unlock()
		}
		return c.Component, err
	}
	s.RUnlock()
	return nil, ErrComponentNotFound
}

func (s *Server) AddComponent(name string, c ComponentState) error {
	if name != "" && c.Component != nil {
		if !c.Initialized && c.NoLazy {
			if err := c.Init(s); err != nil {
				return err
			}
		}
		s.Lock()
		s.components[name] = c
		s.Unlock()
		return nil
	}
	panic("empty name or nil component is not allowed")
}

func (s *Server) RemoveComponent(name string) {
	if name != "" {
		s.Lock()
		if cs, has := s.components[name]; has {
			if cs.Initialized {
				defer s.Unlock()
				cs.Destroy()
				delete(s.components, name)
				return
			}
		}
		delete(s.components, name)
		s.Unlock()
	}
}

// ManageComponent manage those filters used in InterceptHandler, or those added to
// multiple routes, for first condition, ManageComponent used to Init them;
// for second condition, ManageComponent used to avoid multiple call of Init
func (s *Server) ManageComponent(c Component) {
	s.managedComponents = append(s.managedComponents, c)
}

// StartTask start a task synchronously, the value will be passed to task handler
func (s *Server) StartTask(path string, value interface{}) {
	if handler := s.MatchTaskHandler(&url.URL{Path: path}); handler != nil {
		handler.Handle(value)
		return
	}
	panic("No task handler found for " + path)
}

// ServHttp serve for http reuest
// find handler and resolve path, find filters, process
func (s *Server) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if l := len(path); l > 1 && path[l-1] == '/' {
		request.URL.Path = path[:l-1]
	}
	if websocket.IsWebSocketRequest(request) {
		s.serveWebSocket(w, request)
	} else {
		s.serveHTTP(w, request)
	}
}

// serveWebSocket serve for websocket protocal
func (s *Server) serveWebSocket(w http.ResponseWriter, request *http.Request) {
	handler, indexer := s.MatchWebSocketHandler(request.URL)
	if handler == nil {
		w.WriteHeader(http.StatusNotFound)
	} else if conn, err := websocket.UpgradeWebsocket(w, request, s.checker); err == nil {
		handler.Handle(newWebSocketConn(s, conn, indexer))
		indexer.destroySelf()
	} // else connecion will be auto-closed when error occoured,
}

// serveHTTP serve for http protocal
func (s *Server) serveHTTP(w http.ResponseWriter, request *http.Request) {
	url := request.URL
	url.Host = request.Host
	handler, indexer, filters := s.MatchHandlerFilters(url)
	requestEnv := newRequestEnvFromPool()
	res := s.ResourceMaster.Resource(&requestEnv.req)
	req := requestEnv.req.init(s, res, request, indexer)
	resp := requestEnv.resp.init(res, w)

	if s.contentType != _CONTENTTYPE_DISABLE {
		resp.SetContentType(s.contentType)
	}

	var chain FilterChain
	if handler == nil {
		resp.ReportNotFound()
	} else if chain = FilterChain(handler.Handler(req.Method())); chain == nil {
		resp.ReportMethodNotAllowed()
	}

	newFilterChain(s.RootFilters.Filters(url),
		newFilterChain(filters, chain),
	)(req, resp)

	req.destroy()
	resp.destroy()
	recycleRequestEnv(requestEnv)
	recycleFilters(filters)
}

// from net/http/server/go
type tcpKeepAliveListener struct {
	*net.TCPListener
	AlivePeriod int // alive period by minutes
}

func (ln *tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(time.Duration(ln.AlivePeriod) * time.Minute)
	return tc, nil
}

func (s *Server) config(o *ServerOption) {
	o.init()
	log := o.Logger
	s.Log = log

	log.Debugln("ContentType:", o.ContentType)
	s.contentType = o.ContentType
	s.checker = websocket.HeaderChecker(o.WebSocketChecker).HandshakeCheck

	if len(s.ResourceMaster.Resources) == 0 {
		s.ResourceMaster.Default(RES_JSON, JSONResource{})
	}
	log.Debugln("Init resource master")
	s.LogError(s.ResourceMaster.Init(s))

	log.Debugln("VarCountPerRoute:", o.PathVarCount)
	pathVarCount = o.PathVarCount
	log.Debugln("FilterCountPerRoute:", o.FilterCount)
	filterCount = o.FilterCount

	log.Debugln("Init managed components")
	for i := range s.managedComponents {
		s.LogError(s.managedComponents[i].Init(s))
	}

	log.Debugln("Init root filters")
	s.LogError(s.RootFilters.Init(s))
	log.Debugln("Init Handlers and Filters")
	s.LogError(s.Router.Init(s))

	// destroy temporary data store
	tmpDestroy()
	log.Debugln("Server Start:", o.ListenAddr)

	runtime.GC()
}

// LogError will panic goroutine, be care to call this and note to relase resource
// with 'defer'
func (s *Server) LogError(err error) {
	if err != nil {
		s.Log.Errorln(err)
	}
}

// Start start server as http server, if options is nil, use default configurations
func (s *Server) Start(options *ServerOption) error {
	if options == nil {
		options = &ServerOption{}
	}
	s.config(options)
	l, err := s.listen(options)
	if err == nil {
		s.listener = l
		srv := &http.Server{
			ReadTimeout:  time.Duration(options.ReadTimeout) * time.Millisecond,
			WriteTimeout: time.Duration(options.WriteTimeout) * time.Millisecond,
			Handler:      s,
			ConnState:    s.connStateHook,
		}
		err = srv.Serve(l)
	}
	return err
}

func (*Server) listen(options *ServerOption) (net.Listener, error) {
	ln, err := net.Listen("tcp", options.ListenAddr)
	if err == nil {
		ln = &tcpKeepAliveListener{
			TCPListener: ln.(*net.TCPListener),
			AlivePeriod: options.KeepAlivePeriod,
		}

		if options.TLSConfig != nil {
			ln = tls.NewListener(ln, options.TLSConfig)
		} else if options.CertFile != "" {
			// from net/http/server.go.ListenAndServeTLS
			config := &tls.Config{
				NextProtos:   []string{"http/1.1"},
				Certificates: make([]tls.Certificate, 1),
			}
			config.Certificates[0], err = tls.LoadX509KeyPair(options.CertFile, options.KeyFile)
			if err == nil {
				ln = tls.NewListener(ln, config)
			}
		}
	}

	if err != nil && ln != nil {
		ln.Close()
		return nil, err
	}
	return ln, err
}

func (s *Server) connStateHook(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateActive:
		if atomic.LoadInt32(&s.state) == _NORMAL {
			s.activeConns.Add(1)
		} else {
			// previous idle connections before call server.Destroy() becomes active, directly close it
			conn.Close()
		}
	case http.StateIdle:
		if atomic.LoadInt32(&s.state) == _DESTROYED {
			conn.Close()
		}
		s.activeConns.Done()
	case http.StateHijacked:
		s.activeConns.Done()
	}
}

// Destroy stop server, release all resources, if destroyed, server can't be reused,
// instead, create a new one.
// It only wait for managed connections, hijacked/websocket connections is not
func (s *Server) Destroy() {
	if atomic.CompareAndSwapInt32(&s.state, _NORMAL, _DESTROYED) { // signal close idle connections
		s.listener.Close()   // don't accept connections
		s.activeConns.Wait() // wait connections in service to be idle

		// release resources
		s.RootFilters.Destroy()
		s.Router.Destroy()
		for _, c := range s.components {
			c.Destroy()
		}
		for i := range s.managedComponents {
			s.managedComponents[i].Destroy()
		}
	}
}
