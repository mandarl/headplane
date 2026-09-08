package auth

import (
	"encoding/json"
	"testing"
)

const agentDataTestSchema = `
CREATE TABLE service_description_overrides (
	id text PRIMARY KEY,
	host_id text NOT NULL,
	proto text NOT NULL,
	port integer NOT NULL,
	description text NOT NULL,
	updated_by text,
	updated_at integer
);
CREATE TABLE host_info (
	host_id text PRIMARY KEY,
	payload text,
	updated_at integer NOT NULL
);`

func agentDataService(t *testing.T) *Service {
	t.Helper()
	svc := testService(t)
	if _, err := svc.db.Exec(agentDataTestSchema); err != nil {
		t.Fatal(err)
	}
	return svc
}

func strPtr(s string) *string { return &s }

func TestServiceOverridesUpsertGetDelete(t *testing.T) {
	svc := agentDataService(t)

	by := "Mandar"
	if err := svc.SetServiceOverride("host1", "tcp", 80, "Web server", &by); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetServiceOverride("host1", "udp", 53, "DNS", nil); err != nil {
		t.Fatal(err)
	}

	ovs, err := svc.GetServiceOverrides("host1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ovs) != 2 {
		t.Fatalf("expected 2 overrides, got %d", len(ovs))
	}
	web := ovs["tcp:80"]
	if web.Description != "Web server" {
		t.Fatalf("wrong description: %q", web.Description)
	}
	if web.UpdatedBy == nil || *web.UpdatedBy != "Mandar" {
		t.Fatalf("wrong updatedBy: %+v", web.UpdatedBy)
	}
	if web.UpdatedAt == nil || *web.UpdatedAt == "" {
		t.Fatalf("expected updatedAt set, got %+v", web.UpdatedAt)
	}
	dns := ovs["udp:53"]
	if dns.UpdatedBy != nil {
		t.Fatalf("expected nil updatedBy, got %q", *dns.UpdatedBy)
	}

	// Empty description deletes the row.
	if err := svc.SetServiceOverride("host1", "tcp", 80, "", &by); err != nil {
		t.Fatal(err)
	}
	ovs, err = svc.GetServiceOverrides("host1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ovs) != 1 {
		t.Fatalf("expected 1 override after delete, got %d", len(ovs))
	}

	// Other hosts are isolated.
	ovs, err = svc.GetServiceOverrides("host2")
	if err != nil {
		t.Fatal(err)
	}
	if len(ovs) != 0 {
		t.Fatalf("expected 0 overrides for host2, got %d", len(ovs))
	}

	// Upsert overwrites.
	if err := svc.SetServiceOverride("host1", "udp", 53, "DNS v2", &by); err != nil {
		t.Fatal(err)
	}
	ovs, _ = svc.GetServiceOverrides("host1")
	if ovs["udp:53"].Description != "DNS v2" {
		t.Fatalf("expected overwrite, got %q", ovs["udp:53"].Description)
	}
}

func TestHostInfoRoundTripAndPrune(t *testing.T) {
	svc := agentDataService(t)

	payloads := map[string]json.RawMessage{
		"key1": json.RawMessage(`{"hostname":"a"}`),
		"key2": json.RawMessage(`{"hostname":"b"}`),
	}
	if err := svc.UpsertHostInfo(payloads); err != nil {
		t.Fatal(err)
	}

	got, err := svc.LookupHostInfo([]string{"key1", "key2", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 payloads, got %d", len(got))
	}
	if string(got["key1"]) != `{"hostname":"a"}` {
		t.Fatalf("wrong payload: %s", got["key1"])
	}

	// Prune everything but key1.
	deleted, err := svc.PruneHostInfoNotIn([]string{"key1"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted row, got %d", deleted)
	}
	got, _ = svc.LookupHostInfo([]string{"key1", "key2"})
	if len(got) != 1 {
		t.Fatalf("expected 1 payload after prune, got %d", len(got))
	}

	// Empty active set is a no-op.
	deleted, err = svc.PruneHostInfoNotIn(nil)
	if err != nil || deleted != 0 {
		t.Fatalf("expected no-op prune, got %d, %v", deleted, err)
	}
}
