package auth

import (
	"testing"
)

func mustUser(t *testing.T, s *Service, subject string) string {
	t.Helper()
	id, err := s.FindOrCreateUser(subject, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestListUsersAndClaimed(t *testing.T) {
	s := testService(t)
	id1 := mustUser(t, s, "sub-1")
	id2 := mustUser(t, s, "sub-2")

	users, err := s.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("len = %d, want 2", len(users))
	}

	claimed, err := s.ClaimedHeadscaleUserIds()
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed = %v, want empty", claimed)
	}

	if ok, err := s.LinkHeadscaleUser(id1, "hs-1"); err != nil || !ok {
		t.Fatalf("link = %v %v", ok, err)
	}
	claimed, err = s.ClaimedHeadscaleUserIds()
	if err != nil {
		t.Fatal(err)
	}
	if !claimed["hs-1"] || len(claimed) != 1 {
		t.Fatalf("claimed = %v", claimed)
	}

	// Linking the same id to another user is refused.
	if ok, _ := s.LinkHeadscaleUser(id2, "hs-1"); ok {
		t.Fatal("link should have been refused")
	}
}

func TestUnlinkHeadscaleUser(t *testing.T) {
	s := testService(t)
	id := mustUser(t, s, "sub-1")
	if ok, err := s.LinkHeadscaleUser(id, "hs-1"); err != nil || !ok {
		t.Fatal("link failed")
	}
	ok, err := s.UnlinkHeadscaleUser(id)
	if err != nil || !ok {
		t.Fatalf("unlink = %v %v", ok, err)
	}
	u, err := s.GetUserByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if u.HeadscaleUserID != nil {
		t.Fatalf("headscale_user_id = %q, want nil", *u.HeadscaleUserID)
	}
	if ok, _ := s.UnlinkHeadscaleUser("nope"); ok {
		t.Fatal("unlink of missing user should be false")
	}
}

func TestReassignUser(t *testing.T) {
	s := testService(t)
	ownerID := mustUser(t, s, "sub-owner") // first user becomes owner
	memberID := mustUser(t, s, "sub-member")

	ok, err := s.ReassignUser(memberID, "admin")
	if err != nil || !ok {
		t.Fatalf("reassign = %v %v", ok, err)
	}
	u, _ := s.GetUserByID(memberID)
	if u.Role != "admin" {
		t.Fatalf("role = %q", u.Role)
	}

	// The owner role is protected.
	if ok, _ := s.ReassignUser(ownerID, "member"); ok {
		t.Fatal("reassigning the owner should fail")
	}
	// Unknown users fail.
	if ok, _ := s.ReassignUser("nope", "admin"); ok {
		t.Fatal("reassigning unknown user should fail")
	}
}

func TestTransferOwnership(t *testing.T) {
	s := testService(t)
	ownerID := mustUser(t, s, "sub-owner")
	memberID := mustUser(t, s, "sub-member")

	if ok, _ := s.TransferOwnership(ownerID, ownerID); ok {
		t.Fatal("self-transfer should fail")
	}
	if ok, _ := s.TransferOwnership(memberID, ownerID); ok {
		t.Fatal("non-owner transfer should fail")
	}
	if ok, _ := s.TransferOwnership(ownerID, "nope"); ok {
		t.Fatal("transfer to unknown user should fail")
	}

	ok, err := s.TransferOwnership(ownerID, memberID)
	if err != nil || !ok {
		t.Fatalf("transfer = %v %v", ok, err)
	}
	oldOwner, _ := s.GetUserByID(ownerID)
	newOwner, _ := s.GetUserByID(memberID)
	if oldOwner.Role != "admin" || newOwner.Role != "owner" {
		t.Fatalf("roles = %q %q", oldOwner.Role, newOwner.Role)
	}
}
