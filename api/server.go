// Package api contains the over-the-wire gRPC server for PranaDB.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/squareup/pranadb/command"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/conf"
	"github.com/squareup/pranadb/errors"
	"github.com/squareup/pranadb/protolib"
	"github.com/squareup/pranadb/protos/squareup/cash/pranadb/v1/service"
	"github.com/squareup/pranadb/sess"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/emptypb"

	pingerrors "github.com/pingcap/errors" //nolint: depguard
)

// Server over gRPC.
type Server struct {
	lock                 sync.Mutex
	started              bool
	ce                   *command.Executor
	serverAddress        string
	gsrv                 *grpc.Server
	errorSequence        int64
	sessions             sync.Map
	expSessCheckTimer    *time.Timer
	expSessCheckInterval time.Duration
	sessTimeout          time.Duration
	protoRegistry        *protolib.ProtoRegistry
}

func NewAPIServer(ce *command.Executor, protobufs *protolib.ProtoRegistry, cfg conf.Config) *Server {
	return &Server{
		ce:                   ce,
		protoRegistry:        protobufs,
		serverAddress:        cfg.APIServerListenAddresses[cfg.NodeID],
		expSessCheckInterval: cfg.APIServerSessionCheckInterval,
		sessTimeout:          cfg.APIServerSessionTimeout,
	}
}

func (s *Server) Start() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.started {
		return nil
	}
	list, err := net.Listen("tcp", s.serverAddress)
	if err != nil {
		return err
	}
	s.gsrv = grpc.NewServer()
	reflection.Register(s.gsrv)
	service.RegisterPranaDBServiceServer(s.gsrv, s)
	s.started = true
	go s.startServer(list)
	s.scheduleExpiredSessionsCheck()
	return nil
}

func (s *Server) startServer(list net.Listener) {
	err := s.gsrv.Serve(list) //nolint:ifshort
	s.lock.Lock()
	defer s.lock.Unlock()
	s.started = false
	if err != nil {
		log.Errorf("grpc server listen failed: %v", err)
	}
}

func (s *Server) Stop() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if !s.started {
		return nil
	}
	s.gsrv.Stop()
	if s.expSessCheckTimer != nil {
		s.expSessCheckTimer.Stop()
	}
	return nil
}

var _ service.PranaDBServiceServer = &Server{}

func (s *Server) CreateSession(ctx context.Context, _ *emptypb.Empty) (*service.CreateSessionResponse, error) {
	session := s.ce.CreateSession()
	hasher := sha256.New()
	hasher.Write([]byte(session.ID))
	bytes := hasher.Sum(nil)
	sessKey := hex.EncodeToString(bytes)
	entry := &sessionEntry{
		session: session,
	}
	entry.refreshLastAccessedTime()
	s.sessions.Store(sessKey, entry)
	return &service.CreateSessionResponse{SessionId: sessKey}, nil
}

func (s *Server) CloseSession(ctx context.Context, request *service.CloseSessionRequest) (*emptypb.Empty, error) {
	sessEntry, err := s.lookupSession(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	s.sessions.Delete(request.GetSessionId())
	if err := sessEntry.session.Close(); err != nil {
		log.Errorf("failed to close session %+v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) lookupSession(sessionID string) (*sessionEntry, error) {
	v, ok := s.sessions.Load(sessionID)
	if !ok {
		return nil, errors.NewUnknownSessionIDError(sessionID)
	}
	session, ok := v.(*sessionEntry)
	if !ok {
		panic("not a sessionEntry")
	}
	return session, nil
}

func (s *Server) Heartbeat(ctx context.Context, request *service.HeartbeatRequest) (*emptypb.Empty, error) {
	entry, err := s.lookupSession(request.GetSessionId())
	if err == nil && entry != nil {
		entry.refreshLastAccessedTime()
	}
	return &emptypb.Empty{}, err
}

func (s *Server) ExecuteSQLStatement(in *service.ExecuteSQLStatementRequest, stream service.PranaDBService_ExecuteSQLStatementServer) error {

	defer common.PanicHandler()

	entry, err := s.lookupSession(in.GetSessionId())
	if err != nil {
		return err
	}
	session := entry.session

	executor, err := s.ce.ExecuteSQLStatement(session, in.Statement)
	if err != nil {
		log.Errorf("failed to execute statement %+v", err)
		_, ok := err.(errors.PranaError)
		if !ok {
			err = findCause(err)
			e, ok := err.(*pingerrors.Error)
			if ok {
				msg := e.GetMsg()
				return errors.NewInvalidStatementError(msg)
			}
			// For internal errors we don't return internal error messages to the CLI as this would leak
			// server implementation details. Instead we generate a sequence number and add that to the message
			// and log the internal error in the server logs with the sequence number so it can be looked up
			seq := atomic.AddInt64(&s.errorSequence, 1)
			pe := errors.NewInternalError(seq)
			log.Errorf("internal error occurred with sequence number %d\n%v", seq, err)
			return pe
		}
		return err
	}

	// First send column definitions.
	columns := &service.Columns{}
	names := executor.SimpleColNames()
	for i, typ := range executor.ColTypes() {
		name := names[i]
		column := &service.Column{
			Name: name,
			Type: service.ColumnType(typ.Type),
		}
		if typ.Type == common.TypeDecimal {
			column.DecimalParams = &service.DecimalParams{
				DecimalPrecision: uint32(typ.DecPrecision),
				DecimalScale:     uint32(typ.DecScale),
			}
		}
		columns.Columns = append(columns.Columns, column)
	}
	if err := stream.Send(&service.ExecuteSQLStatementResponse{Result: &service.ExecuteSQLStatementResponse_Columns{Columns: columns}}); err != nil {
		return errors.WithStack(err)
	}

	// Then start sending pages until complete.
	limit := int(in.PageSize)
	for {
		// Transcode rows.
		rows, err := executor.GetRows(limit)
		if err != nil {
			return errors.WithStack(err)
		}
		numRows := rows.RowCount()
		results := &service.Page{
			Count: uint64(numRows),
			Rows:  rows.Serialize(),
		}
		if err = stream.Send(&service.ExecuteSQLStatementResponse{Result: &service.ExecuteSQLStatementResponse_Page{Page: results}}); err != nil {
			return errors.WithStack(err)
		}
		if numRows < limit {
			break
		}
	}
	return nil
}

func (s *Server) RegisterProtobufs(ctx context.Context, request *service.RegisterProtobufsRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.protoRegistry.RegisterFiles(request.GetDescriptors())
}

type sessionEntry struct {
	session          *sess.Session
	lastAccessedTime atomic.Value
}

func (se *sessionEntry) getLastAccessedTime() *time.Time {
	v := se.lastAccessedTime.Load()
	if v == nil {
		panic("no lastAccessedTime")
	}
	lat, ok := v.(*time.Time)
	if !ok {
		panic("not a *time.Time")
	}
	return lat
}

func (se *sessionEntry) refreshLastAccessedTime() {
	t := time.Now()
	se.lastAccessedTime.Store(&t)
}

func (s *Server) scheduleExpiredSessionsCheck() {
	s.expSessCheckTimer = time.AfterFunc(s.expSessCheckInterval, s.checkExpiredSessions)
}

func (s *Server) checkExpiredSessions() {
	s.lock.Lock()
	defer s.lock.Unlock()
	if !s.started {
		return
	}
	now := time.Now()
	s.sessions.Range(func(key, value interface{}) bool {
		se, ok := value.(*sessionEntry)
		if !ok {
			panic("not a sessionEntry")
		}

		lat := se.getLastAccessedTime()
		if now.Sub(*lat) > s.sessTimeout {
			log.Debugf("Deleting expired session %v", key)
			s.sessions.Delete(key)
			if err := se.session.Close(); err != nil {
				log.Errorf("failed to close session %+v", err)
			}
		}
		return true
	})
	s.scheduleExpiredSessionsCheck()
}

func (s *Server) SessionCount() int {
	count := 0
	s.sessions.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

func (s *Server) GetListenAddress() string {
	return s.serverAddress
}

// standard cause recursion is broken for pingcap errors so we have to do it ourselves
func findCause(err error) error {
	type causer interface {
		Cause() error
	}

	for err != nil {
		cause, ok := err.(causer)
		if !ok {
			break
		}
		// pingcap cause can return nil
		newErr := cause.Cause()
		if newErr == nil {
			return err
		}
		err = newErr
	}
	return err
}
