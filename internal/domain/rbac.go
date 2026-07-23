package domain

// StaffRole is a user's role within ONE restaurant's staff roster, stored on
// RestaurantManager.Role (migration 0012). It is orthogonal to the global
// User.Role: RoleAdmin ("superadmin") is a global role with no restaurant
// scope at all — every call site checks actor.Role == RoleAdmin FIRST and
// bypasses this matrix entirely (spec: "суперадмин — может всё", see
// restaurants.ManagerUseCase.authorizeStaffManage and
// usecase/payments.authorizeStaffPermission for the two call sites).
type StaffRole string

const (
	StaffRoleOwner   StaffRole = "owner"
	StaffRoleManager StaffRole = "manager"
	StaffRoleHostess StaffRole = "hostess"
)

// Valid reports whether s is one of the three known staff roles.
func (s StaffRole) Valid() bool {
	switch s {
	case StaffRoleOwner, StaffRoleManager, StaffRoleHostess:
		return true
	}
	return false
}

// rank orders staff roles from least to most privileged. An unknown role
// ranks below every real one (0), so Outranks against it is always false —
// there is no privilege to compare.
func (s StaffRole) rank() int {
	switch s {
	case StaffRoleHostess:
		return 1
	case StaffRoleManager:
		return 2
	case StaffRoleOwner:
		return 3
	default:
		return 0
	}
}

// Outranks reports whether s is STRICTLY more privileged than other. Used
// only to enforce "cannot grant a role at or above your own" when a
// restaurant's own owner manages their staff roster (spec: "нельзя повысить
// роль выше своей") — a superadmin bypasses this check entirely, same as
// every other restaurant-scoped rule here.
func (s StaffRole) Outranks(other StaffRole) bool { return s.rank() > other.rank() }

// Permission is a single, concrete action gated by the RBAC matrix — e.g.
// "refund a payment" — never a role name. Every call site asks
// StaffRole.HasPermission(perm), which keeps the actual role→action mapping
// in exactly ONE place (staffPermissions below) instead of scattered role
// comparisons across usecases (spec: "матрица роль→права — в коде, не в БД,
// чтобы нельзя было случайно выдать лишнее через запись в таблицу").
type Permission string

const (
	// PermBookingManage covers every staff-side booking transition: accept /
	// confirm, reject, seat (Arrive), mark a no-show, cancel, waitlist —
	// the venue's everyday booking operations (spec: "принимает/подтверждает
	// бронь, посадка, неявка"). Granted to every staff role.
	PermBookingManage Permission = "booking.manage"

	// PermPaymentCapture covers charging a hold on seating (CaptureOnSeating)
	// and releasing a hold on rejection (VoidOnRejection) — the payment side
	// effect of the same everyday booking flow above, not an independent
	// money decision. Granted to every staff role, same as PermBookingManage.
	PermPaymentCapture Permission = "payment.capture"

	// PermPaymentRefund covers settling a cancellation or a no-show into its
	// final refund/forfeit split (RefundUseCase.Settle) — an explicit,
	// standalone money-moving decision (spec: "оформляет возврат платежа").
	// Manager and owner only — a hostess must NOT be able to do this.
	PermPaymentRefund Permission = "payment.refund"

	// PermStaffManage covers creating, re-role-ing and removing a
	// restaurant's own manager/hostess accounts (spec: "создаёт и удаляет
	// менеджеров и хостес"). Owner only among restaurant staff roles.
	PermStaffManage Permission = "staff.manage"
)

// staffPermissions is the ENTIRE role→permission matrix. Deliberately a Go
// literal, not a database table, per the spec's data-integrity requirement
// above — there is no write path that can widen it at runtime.
var staffPermissions = map[StaffRole]map[Permission]bool{
	StaffRoleHostess: {
		PermBookingManage:  true,
		PermPaymentCapture: true,
	},
	StaffRoleManager: {
		PermBookingManage:  true,
		PermPaymentCapture: true,
		PermPaymentRefund:  true,
	},
	StaffRoleOwner: {
		PermBookingManage:  true,
		PermPaymentCapture: true,
		PermPaymentRefund:  true,
		PermStaffManage:    true,
	},
}

// HasPermission reports whether staff role s may perform perm. An unknown or
// empty role has no permissions — there is no implicit allow.
func (s StaffRole) HasPermission(perm Permission) bool {
	return staffPermissions[s][perm]
}
