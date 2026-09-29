package auth

import "testing"

// TestRoleBitmasks pins the exact numeric role values from
// app/server/web/roles.ts. If these fail, the Go port drifted from the TS
// source of truth.
func TestRoleBitmasks(t *testing.T) {
	want := map[Role]Capabilities{
		RoleOwner:        65535,
		RoleAdmin:        32767,
		RoleNetworkAdmin: 30015,
		RoleITAdmin:      8139,
		RoleAuditor:      66859,
		RoleViewer:       66817,
		RoleMember:       0,
	}
	for role, w := range want {
		if got := Roles[role]; got != w {
			t.Errorf("role %q = %d, want %d", role, got, w)
		}
	}
}

func TestCapsForRoleFallback(t *testing.T) {
	if CapsForRole("owner") != Roles[RoleOwner] {
		t.Error("CapsForRole(owner) mismatch")
	}
	if CapsForRole("nope") != 0 {
		t.Error("unknown role should fall back to member (0)")
	}
	if NormalizeRole("admin") != RoleAdmin {
		t.Error("NormalizeRole(admin) mismatch")
	}
	if NormalizeRole("bogus") != RoleMember {
		t.Error("unknown role should normalize to member")
	}
}

func TestCan(t *testing.T) {
	svc := &Service{}
	apiKey := &Principal{Kind: "api_key"}
	if !svc.Can(apiKey, CapOwner|CapWritePolicy) {
		t.Error("api_key principal should bypass all checks")
	}
	viewer := &Principal{Kind: "oidc", Role: RoleViewer}
	if !svc.Can(viewer, CapReadMachines) {
		t.Error("viewer should read machines")
	}
	if svc.Can(viewer, CapWriteMachines) {
		t.Error("viewer should not write machines")
	}
	if !svc.Can(viewer, CapReadMachines|CapReadUsers) {
		t.Error("viewer should satisfy combined read caps")
	}
	if svc.Can(viewer, CapReadMachines|CapWriteUsers) {
		t.Error("combined caps must all be present")
	}
}

func TestCanManageNode(t *testing.T) {
	svc := &Service{}
	apiKey := &Principal{Kind: "api_key"}
	if !svc.CanManageNode(apiKey, nil) {
		t.Error("api_key should manage any node")
	}
	admin := &Principal{Kind: "oidc", Role: RoleAdmin}
	if !svc.CanManageNode(admin, nil) {
		t.Error("admin (write_machines) should manage any node")
	}
	hsID := "42"
	owner := &Principal{Kind: "oidc", Role: RoleViewer, HeadscaleUserID: &hsID}
	if !svc.CanManageNode(owner, &hsID) {
		t.Error("user should manage their own node")
	}
	other := "43"
	if svc.CanManageNode(owner, &other) {
		t.Error("user should not manage another user's node")
	}
	if svc.CanManageNode(owner, nil) {
		t.Error("user should not manage a node with no owner id")
	}
	noLink := &Principal{Kind: "oidc", Role: RoleViewer}
	if svc.CanManageNode(noLink, &hsID) {
		t.Error("unlinked user should not manage the node")
	}
}
