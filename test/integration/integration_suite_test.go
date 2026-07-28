// Package integration contains the Ginkgo/Gomega BDD integration suite.
// It exercises the REST and gRPC transports together against a shared store,
// verifying that both APIs stay consistent with one another.
package integration

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestIntegration is the single Go test entry point that runs the whole
// Ginkgo suite: go test ./test/integration/
func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DocVerify Integration Suite")
}
