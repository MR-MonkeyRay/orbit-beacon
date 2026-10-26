package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"orbitbeacon/internal/agent/identity"
	"orbitbeacon/internal/agent/service"
	v1 "orbitbeacon/internal/protocol/v1"
	"orbitbeacon/internal/server/nodes"
)

func TestDirectReportConfirmsLostEnrollmentBindingBeforeRotation(t *testing.T) {
	f := newAgentHTTP(t, 1)
	state, cfg, path := clientState(t)
	ctx := context.Background()
	node, err := f.nodes.CreatePending(ctx, nodes.CreateRequest{Name: "lost enrollment response"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := state.Current()
	if _, err := f.enrollments.Enroll(ctx, ticket.Token, v1.EnrollmentRequest{ProtocolVersion: 1, InstallationID: before.InstallationID, NodeToken: before.NodeToken}); err != nil {
		t.Fatal(err)
	}
	run, err := state.BeginRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(f.handler)
	defer server.Close()
	client, err := service.NewClient(service.ClientOptions{ServerURL: server.URL, HTTPClient: server.Client(), Identity: state, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	report := mustReport(t, reportBody(run.RunGeneration, 1, run.SessionID, 20))
	if _, err := client.Report(ctx, report); err != nil {
		t.Fatal(err)
	}
	durable, err := identity.Open(t.Context(), path, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if durable.Current().NodeID != node.ID {
		t.Fatal("successful report left durable identity unbound")
	}
	rotation, err := f.enrollments.CreateSingle(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Enroll(ctx, rotation.Token); err != nil {
		t.Fatal(err)
	}
	if state.Current().NodeToken == before.NodeToken {
		t.Fatal("explicit rotation only recovered existing binding")
	}
	assertHTTPStatus(t, agentRequest(t, f, "/api/agent/v1/report", before.NodeToken, reportBody(run.RunGeneration, 2, run.SessionID, 20)), http.StatusUnauthorized)
}
