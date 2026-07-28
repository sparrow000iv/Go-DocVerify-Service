package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/sparrow000iv/go-docverify-service/internal/grpcapi"
	"github.com/sparrow000iv/go-docverify-service/internal/httpapi"
	"github.com/sparrow000iv/go-docverify-service/internal/store"
	pb "github.com/sparrow000iv/go-docverify-service/proto/docverify/v1"
)

var _ = Describe("DocVerify Service", func() {
	var (
		st         *store.Store
		httpServer *httptest.Server
		grpcClient pb.DocVerifyClient
		grpcSrv    *grpc.Server
		lis        *bufconn.Listener
		conn       *grpc.ClientConn
		ctx        context.Context
	)

	// A fresh service is built for every spec, so specs never leak state.
	BeforeEach(func() {
		ctx = context.Background()
		st = store.New()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		httpServer = httptest.NewServer(httpapi.New(st, logger).Routes())

		lis = bufconn.Listen(1024 * 1024)
		grpcSrv = grpc.NewServer()
		pb.RegisterDocVerifyServer(grpcSrv, grpcapi.New(st))
		go func() {
			defer GinkgoRecover()
			_ = grpcSrv.Serve(lis)
		}()

		var err error
		conn, err = grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		Expect(err).NotTo(HaveOccurred())
		grpcClient = pb.NewDocVerifyClient(conn)
	})

	AfterEach(func() {
		conn.Close()
		grpcSrv.Stop()
		lis.Close()
		httpServer.Close()
	})

	// --- helpers -----------------------------------------------------------

	postDoc := func(body string) (*http.Response, map[string]any) {
		resp, err := http.Post(httpServer.URL+"/api/v1/documents",
			"application/json", bytes.NewBufferString(body))
		Expect(err).NotTo(HaveOccurred())
		var out map[string]any
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_ = json.Unmarshal(raw, &out)
		return resp, out
	}

	// --- specs -------------------------------------------------------------

	Describe("Health and observability endpoints", func() {
		It("reports healthy on /healthz", func() {
			resp, err := http.Get(httpServer.URL + "/healthz")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
		})

		It("reports ready on /readyz", func() {
			resp, err := http.Get(httpServer.URL + "/readyz")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
		})

		It("exposes Prometheus metrics", func() {
			_, _ = http.Get(httpServer.URL + "/healthz")

			resp, err := http.Get(httpServer.URL + "/metrics")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			body, _ := io.ReadAll(resp.Body)
			Expect(string(body)).To(ContainSubstring("docverify_http_requests_total"))
		})
	})

	Describe("Submitting documents over REST", func() {
		Context("with a valid payload", func() {
			It("creates a pending document", func() {
				resp, doc := postDoc(`{"owner":"tushar","doc_type":"passport","content":"payload"}`)
				Expect(resp.StatusCode).To(Equal(http.StatusCreated))
				Expect(doc).To(HaveKeyWithValue("status", "PENDING"))
				Expect(doc).To(HaveKeyWithValue("owner", "tushar"))
				Expect(doc["id"]).NotTo(BeEmpty())
			})
		})

		Context("with invalid payloads", func() {
			DescribeTable("rejects the request with 400",
				func(body string) {
					resp, _ := postDoc(body)
					Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
				},
				Entry("empty owner", `{"owner":"","doc_type":"passport","content":"x"}`),
				Entry("empty doc_type", `{"owner":"t","doc_type":"","content":"x"}`),
				Entry("empty content", `{"owner":"t","doc_type":"passport","content":""}`),
				Entry("unsupported type", `{"owner":"t","doc_type":"library_card","content":"x"}`),
				Entry("malformed JSON", `{"owner":`),
				Entry("unknown field", `{"owner":"t","doc_type":"passport","content":"x","x":1}`),
			)
		})
	})

	Describe("Cross-transport consistency", func() {
		It("reads a REST-created document over gRPC", func() {
			_, created := postDoc(`{"owner":"tushar","doc_type":"passport","content":"payload"}`)
			id, _ := created["id"].(string)
			Expect(id).NotTo(BeEmpty())

			got, err := grpcClient.Get(ctx, &pb.GetRequest{Id: id})
			Expect(err).NotTo(HaveOccurred())
			Expect(got.GetId()).To(Equal(id))
			Expect(got.GetOwner()).To(Equal("tushar"))
			Expect(got.GetStatus()).To(Equal(pb.Status_STATUS_PENDING))
		})

		It("reads a gRPC-created document over REST", func() {
			created, err := grpcClient.Submit(ctx, &pb.SubmitRequest{
				Owner: "asha", DocType: "national_id", Content: "payload",
			})
			Expect(err).NotTo(HaveOccurred())

			resp, err := http.Get(httpServer.URL + "/api/v1/documents/" + created.GetId())
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var doc map[string]any
			raw, _ := io.ReadAll(resp.Body)
			Expect(json.Unmarshal(raw, &doc)).To(Succeed())
			Expect(doc).To(HaveKeyWithValue("owner", "asha"))
		})

		It("reflects a gRPC verification in the REST view", func() {
			_, created := postDoc(`{"owner":"t","doc_type":"passport","content":"payload"}`)
			id, _ := created["id"].(string)

			verified, err := grpcClient.Verify(ctx, &pb.VerifyRequest{Id: id})
			Expect(err).NotTo(HaveOccurred())
			Expect(verified.GetStatus()).NotTo(Equal(pb.Status_STATUS_PENDING))

			resp, err := http.Get(httpServer.URL + "/api/v1/documents/" + id)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			var doc map[string]any
			raw, _ := io.ReadAll(resp.Body)
			Expect(json.Unmarshal(raw, &doc)).To(Succeed())
			Expect(doc["status"]).To(BeElementOf("VERIFIED", "REJECTED"))
		})
	})

	Describe("Verification lifecycle", func() {
		It("moves a document to a terminal status", func() {
			_, created := postDoc(`{"owner":"t","doc_type":"utility_bill","content":"payload"}`)
			id, _ := created["id"].(string)

			resp, err := http.Post(httpServer.URL+"/api/v1/documents/"+id+"/verify",
				"application/json", nil)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			var doc map[string]any
			raw, _ := io.ReadAll(resp.Body)
			Expect(json.Unmarshal(raw, &doc)).To(Succeed())
			Expect(doc["status"]).To(BeElementOf("VERIFIED", "REJECTED"))
		})

		It("is idempotent across repeated verifications", func() {
			created, err := grpcClient.Submit(ctx, &pb.SubmitRequest{
				Owner: "t", DocType: "passport", Content: "payload",
			})
			Expect(err).NotTo(HaveOccurred())

			first, err := grpcClient.Verify(ctx, &pb.VerifyRequest{Id: created.GetId()})
			Expect(err).NotTo(HaveOccurred())
			second, err := grpcClient.Verify(ctx, &pb.VerifyRequest{Id: created.GetId()})
			Expect(err).NotTo(HaveOccurred())

			Expect(second.GetStatus()).To(Equal(first.GetStatus()))
			Expect(second.GetScore()).To(Equal(first.GetScore()))
		})
	})

	Describe("Error handling", func() {
		It("returns 404 over REST for a missing document", func() {
			resp, err := http.Get(httpServer.URL + "/api/v1/documents/missing")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})

		It("returns NOT_FOUND over gRPC for a missing document", func() {
			_, err := grpcClient.Get(ctx, &pb.GetRequest{Id: "missing"})
			Expect(status.Code(err)).To(Equal(codes.NotFound))
		})

		It("returns INVALID_ARGUMENT over gRPC for bad input", func() {
			_, err := grpcClient.Submit(ctx, &pb.SubmitRequest{
				Owner: "", DocType: "passport", Content: "x",
			})
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})
	})

	Describe("Listing and filtering", func() {
		BeforeEach(func() {
			for i := 0; i < 5; i++ {
				_, err := grpcClient.Submit(ctx, &pb.SubmitRequest{
					Owner: "t", DocType: "passport", Content: fmt.Sprintf("content-%d", i),
				})
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("returns every document", func() {
			out, err := grpcClient.List(ctx, &pb.ListRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.GetTotal()).To(BeEquivalentTo(5))
		})

		It("honours the limit without changing the total", func() {
			out, err := grpcClient.List(ctx, &pb.ListRequest{Limit: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.GetDocuments()).To(HaveLen(2))
			Expect(out.GetTotal()).To(BeEquivalentTo(5))
		})

		It("rejects an invalid status filter over REST", func() {
			resp, err := http.Get(httpServer.URL + "/api/v1/documents?status=BOGUS")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})
	})

	Describe("Concurrency and resiliency", func() {
		It("handles concurrent submissions from both transports safely", func() {
			const n = 30
			var wg sync.WaitGroup
			wg.Add(n * 2)

			for i := 0; i < n; i++ {
				go func(i int) {
					defer GinkgoRecover()
					defer wg.Done()
					_, _ = postDoc(fmt.Sprintf(
						`{"owner":"rest","doc_type":"passport","content":"rest-%d"}`, i))
				}(i)

				go func(i int) {
					defer GinkgoRecover()
					defer wg.Done()
					_, err := grpcClient.Submit(ctx, &pb.SubmitRequest{
						Owner: "grpc", DocType: "passport", Content: fmt.Sprintf("grpc-%d", i),
					})
					Expect(err).NotTo(HaveOccurred())
				}(i)
			}
			wg.Wait()

			Expect(st.Len()).To(Equal(n * 2))
		})

		It("stays consistent when verifying many documents concurrently", func() {
			ids := make([]string, 0, 20)
			for i := 0; i < 20; i++ {
				d, err := grpcClient.Submit(ctx, &pb.SubmitRequest{
					Owner: "t", DocType: "passport", Content: fmt.Sprintf("c-%d", i),
				})
				Expect(err).NotTo(HaveOccurred())
				ids = append(ids, d.GetId())
			}

			var wg sync.WaitGroup
			wg.Add(len(ids))
			for _, id := range ids {
				go func(id string) {
					defer GinkgoRecover()
					defer wg.Done()
					_, err := grpcClient.Verify(ctx, &pb.VerifyRequest{Id: id})
					Expect(err).NotTo(HaveOccurred())
				}(id)
			}
			wg.Wait()

			out, err := grpcClient.List(ctx, &pb.ListRequest{StatusFilter: pb.Status_STATUS_PENDING})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.GetTotal()).To(BeZero(), "no document should remain pending")
		})
	})
})
