package auth

// Capabilities mirrors app/server/web/roles.ts exactly. Values are the
// source of truth for the role bitmasks stored in the users table.
type Capabilities uint32

const (
	CapUIAccess          Capabilities = 1 << 0
	CapReadPolicy        Capabilities = 1 << 1
	CapWritePolicy       Capabilities = 1 << 2
	CapReadNetwork       Capabilities = 1 << 3
	CapWriteNetwork      Capabilities = 1 << 4
	CapReadFeature       Capabilities = 1 << 5
	CapWriteFeature      Capabilities = 1 << 6
	CapConfigureIAM      Capabilities = 1 << 7
	CapReadMachines      Capabilities = 1 << 8
	CapWriteMachines     Capabilities = 1 << 9
	CapReadUsers         Capabilities = 1 << 10
	CapWriteUsers        Capabilities = 1 << 11
	CapGenerateAuthKeys  Capabilities = 1 << 12
	CapUseTags           Capabilities = 1 << 13
	CapWriteTailnet      Capabilities = 1 << 14
	CapOwner             Capabilities = 1 << 15
	CapGenerateOwnAuthKs Capabilities = 1 << 16
)

// Role mirrors the TS Role union.
type Role string

const (
	RoleOwner        Role = "owner"
	RoleAdmin        Role = "admin"
	RoleNetworkAdmin Role = "network_admin"
	RoleITAdmin      Role = "it_admin"
	RoleAuditor      Role = "auditor"
	RoleViewer       Role = "viewer"
	RoleMember       Role = "member"
)

// Roles mirrors the TS Roles record, built from the same capability
// combinations so any drift is caught by roles_test.go.
var Roles = map[Role]Capabilities{
	RoleOwner: CapUIAccess | CapReadPolicy | CapWritePolicy | CapReadNetwork |
		CapWriteNetwork | CapReadFeature | CapWriteFeature | CapConfigureIAM |
		CapReadMachines | CapWriteMachines | CapReadUsers | CapWriteUsers |
		CapGenerateAuthKeys | CapUseTags | CapWriteTailnet | CapOwner,
	RoleAdmin: CapUIAccess | CapReadPolicy | CapWritePolicy | CapReadNetwork |
		CapWriteNetwork | CapReadFeature | CapWriteFeature | CapConfigureIAM |
		CapReadMachines | CapWriteMachines | CapReadUsers | CapWriteUsers |
		CapGenerateAuthKeys | CapUseTags | CapWriteTailnet,
	RoleNetworkAdmin: CapUIAccess | CapReadPolicy | CapWritePolicy | CapReadNetwork |
		CapWriteNetwork | CapReadFeature | CapReadMachines | CapReadUsers |
		CapGenerateAuthKeys | CapUseTags | CapWriteTailnet,
	RoleITAdmin: CapUIAccess | CapReadPolicy | CapReadNetwork | CapWriteFeature |
		CapConfigureIAM | CapReadMachines | CapWriteMachines | CapReadUsers |
		CapWriteUsers | CapGenerateAuthKeys,
	RoleAuditor: CapUIAccess | CapReadPolicy | CapReadNetwork | CapReadFeature |
		CapReadMachines | CapReadUsers | CapGenerateOwnAuthKs,
	RoleViewer: CapUIAccess | CapReadMachines | CapReadUsers | CapGenerateOwnAuthKs,
	RoleMember: 0,
}

// CapsForRole mirrors capsForRole. Unknown roles fall back to member,
// matching the TS resolve() behavior (`user.role in Roles ? ... : "member"`).
func CapsForRole(role string) Capabilities {
	if caps, ok := Roles[Role(role)]; ok {
		return caps
	}
	return Roles[RoleMember]
}

// NormalizeRole returns role when it is a known role, else "member".
func NormalizeRole(role string) Role {
	if _, ok := Roles[Role(role)]; ok {
		return Role(role)
	}
	return RoleMember
}
