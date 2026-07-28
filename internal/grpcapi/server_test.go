package grpcapi

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/sparrow000iv/go-docverify-service/internal/store"
	pb "github.com/sparrow000iv/go-docverify-service/proto/docverify/v1"
)

// newTestClient spins up an in-process gRPC server over bufconn, avoiding
// real TCP ports so tests stay hermetic and fast.
func newTestClient(t *testing.T) pb.DocVerifyClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	pb.RegisterDocVerifyServer(srv, New(store.New()))

	go func() {
		if err := srv.Serve(lis); err != nil {
			return
		}
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}

	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	})
	return pb.NewDocVerifyClient(conn)
}

func TestGRPC_SubmitValidation(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		req      *pb.SubmitRequest
		wantCode codes.Code
	}{
		{"valid", &pb.SubmitRequest{Owner: "tushar", DocType: "passport", Content: "x"}, codes.OK},
		{"empty owner", &pb.SubmitRequest{Owner: "", DocType: "passport", Content: "x"}, codes.InvalidArgument},
		{"empty type", &pb.SubmitRequest{Owner: "t", DocType: "", Content: "x"}, codes.InvalidArgument},
		{"empty content", &pb.SubmitRequest{Owner: "t", DocType: "passport", Content: ""}, codes.InvalidArgument},
		{"bad type", &pb.SubmitRequest{Owner: "t", DocType: "library_card", Content: "x"}, codes.InvalidArgument},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Submit(ctx, tc.req)
			if got := status.Code(err); got != tc.wantCode {
				t.Errorf("code = %v, want %v (err=%v)", got, tc.wantCode, err)
			}
		})
	}
}

func TestGRPC_Lifecycle(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ctx := context.Background()

	created, err := client.Submit(ctx, &pb.SubmitRequest{
		Owner: "tushar", DocType: "passport", Content: "payload",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if created.GetStatus() != pb.Status_STATUS_PENDING {
		t.Fatalf("status = %v, want PENDING", created.GetStatus())
	}

	got, err := client.Get(ctx, &pb.GetRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetId() != created.GetId() {
		t.Errorf("id = %q, want %q", got.GetId(), created.GetId())
	}

	verified, err := client.Verify(ctx, &pb.VerifyRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified.GetStatus() == pb.Status_STATUS_PENDING {
		t.Error("status still PENDING after Verify")
	}

	del, err := client.Delete(ctx, &pb.DeleteRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !del.GetDeleted() {
		t.Error("Deleted = false, want true")
	}

	if _, err := client.Get(ctx, &pb.GetRequest{Id: created.GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("Get after delete = %v, want NotFound", status.Code(err))
	}
}

func TestGRPC_NotFoundCodes(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ctx := context.Background()

	if _, err := client.Get(ctx, &pb.GetRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Get = %v, want NotFound", status.Code(err))
	}
	if _, err := client.Verify(ctx, &pb.VerifyRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Verify = %v, want NotFound", status.Code(err))
	}
	if _, err := client.Delete(ctx, &pb.DeleteRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Delete = %v, want NotFound", status.Code(err))
	}
}

func TestGRPC_ListFilterAndLimit(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		if _, err := client.Submit(ctx, &pb.SubmitRequest{
			Owner: "t", DocType: "passport", Content: string(rune('a' + i)),
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	all, err := client.List(ctx, &pb.ListRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if all.GetTotal() != 6 {
		t.Errorf("total = %d, want 6", all.GetTotal())
	}

	limited, err := client.List(ctx, &pb.ListRequest{Limit: 2})
	if err != nil {
		t.Fatalf("List(limit=2): %v", err)
	}
	if len(limited.GetDocuments()) != 2 {
		t.Errorf("len = %d, want 2", len(limited.GetDocuments()))
	}
	if limited.GetTotal() != 6 {
		t.Errorf("total = %d, want 6 (limit must not change total)", limited.GetTotal())
	}

	pending, err := client.List(ctx, &pb.ListRequest{StatusFilter: pb.Status_STATUS_PENDING})
	if err != nil {
		t.Fatalf("List(pending): %v", err)
	}
	if pending.GetTotal() != 6 {
		t.Errorf("pending total = %d, want 6", pending.GetTotal())
	}
}

func TestGRPC_VerifyIsIdempotent(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	ctx := context.Background()

	created, err := client.Submit(ctx, &pb.SubmitRequest{
		Owner: "t", DocType: "national_id", Content: "payload",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	first, err := client.Verify(ctx, &pb.VerifyRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	second, err := client.Verify(ctx, &pb.VerifyRequest{Id: created.GetId()})
	if err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	if first.GetStatus() != second.GetStatus() || first.GetScore() != second.GetScore() {
		t.Errorf("not idempotent: %v/%v vs %v/%v",
			first.GetStatus(), first.GetScore(), second.GetStatus(), second.GetScore())
	}
}
