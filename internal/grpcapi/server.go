// Package grpcapi implements the gRPC transport for the DocVerify service.
// It shares the same store as the REST layer, so both APIs are always
// consistent with one another.
package grpcapi

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/sparrow000iv/go-docverify-service/internal/metrics"
	"github.com/sparrow000iv/go-docverify-service/internal/store"
	pb "github.com/sparrow000iv/go-docverify-service/proto/docverify/v1"
)

var _ = emptypb.Empty{} // keep protobuf well-known types linked

// Server implements pb.DocVerifyServer.
type Server struct {
	pb.UnimplementedDocVerifyServer
	store *store.Store
}

// New returns a gRPC server backed by the given store.
func New(s *store.Store) *Server { return &Server{store: s} }

// grpcCode maps domain errors onto gRPC status codes.
func grpcCode(err error) codes.Code {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return codes.NotFound
	case errors.Is(err, store.ErrInvalidOwner),
		errors.Is(err, store.ErrInvalidType),
		errors.Is(err, store.ErrInvalidConten),
		errors.Is(err, store.ErrUnsupported):
		return codes.InvalidArgument
	default:
		return codes.Internal
	}
}

// toProto converts a domain document into its wire representation.
func toProto(d store.Document) *pb.Document {
	return &pb.Document{
		Id:        d.ID,
		Owner:     d.Owner,
		DocType:   d.DocType,
		Status:    toProtoStatus(d.Status),
		Score:     d.Score,
		CreatedAt: d.CreatedAt.Format(time.RFC3339),
		UpdatedAt: d.UpdatedAt.Format(time.RFC3339),
	}
}

func toProtoStatus(s store.Status) pb.Status {
	switch s {
	case store.StatusPending:
		return pb.Status_STATUS_PENDING
	case store.StatusVerified:
		return pb.Status_STATUS_VERIFIED
	case store.StatusRejected:
		return pb.Status_STATUS_REJECTED
	default:
		return pb.Status_STATUS_UNSPECIFIED
	}
}

func fromProtoStatus(s pb.Status) store.Status {
	switch s {
	case pb.Status_STATUS_PENDING:
		return store.StatusPending
	case pb.Status_STATUS_VERIFIED:
		return store.StatusVerified
	case pb.Status_STATUS_REJECTED:
		return store.StatusRejected
	default:
		return ""
	}
}

// observe records metrics for a completed call.
func observe(method string, started time.Time, err error) error {
	code := codes.OK
	if err != nil {
		code = grpcCode(err)
	}
	metrics.ObserveGRPC(method, code.String(), started)
	if err != nil {
		return status.Error(code, err.Error())
	}
	return nil
}

// Submit queues a document for verification.
func (s *Server) Submit(_ context.Context, req *pb.SubmitRequest) (*pb.Document, error) {
	started := time.Now()
	doc, err := s.store.Submit(req.GetOwner(), req.GetDocType(), req.GetContent())
	if e := observe("Submit", started, err); e != nil {
		return nil, e
	}
	metrics.DocumentsStored.Set(float64(s.store.Len()))
	return toProto(doc), nil
}

// Get returns a document by id.
func (s *Server) Get(_ context.Context, req *pb.GetRequest) (*pb.Document, error) {
	started := time.Now()
	doc, err := s.store.Get(req.GetId())
	if e := observe("Get", started, err); e != nil {
		return nil, e
	}
	return toProto(doc), nil
}

// List returns documents, optionally filtered by status.
func (s *Server) List(_ context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	started := time.Now()
	docs, total := s.store.List(fromProtoStatus(req.GetStatusFilter()), int(req.GetLimit()))
	out := make([]*pb.Document, 0, len(docs))
	for _, d := range docs {
		out = append(out, toProto(d))
	}
	_ = observe("List", started, nil)
	return &pb.ListResponse{Documents: out, Total: int32(total)}, nil
}

// Verify scores a stored document and returns its terminal state.
func (s *Server) Verify(_ context.Context, req *pb.VerifyRequest) (*pb.Document, error) {
	started := time.Now()
	doc, err := s.store.Verify(req.GetId())
	if e := observe("Verify", started, err); e != nil {
		return nil, e
	}
	metrics.DocumentsTotal.WithLabelValues(string(doc.Status)).Inc()
	return toProto(doc), nil
}

// Delete removes a document by id.
func (s *Server) Delete(_ context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	started := time.Now()
	err := s.store.Delete(req.GetId())
	if e := observe("Delete", started, err); e != nil {
		return nil, e
	}
	metrics.DocumentsStored.Set(float64(s.store.Len()))
	return &pb.DeleteResponse{Deleted: true}, nil
}
