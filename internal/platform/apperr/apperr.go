// Package apperr adalah satu-satunya tempat error domain didefinisikan bersama HTTP status
// dan `code`-nya. Sumber kebenaran: docs/design/15-api-error-catalogue.md. Kode lain
// mengembalikan *apperr.Error (atau membungkusnya); lapisan HTTP mengubahnya menjadi RFC 9457.
package apperr

import (
	"errors"
	"net/http"
)

// FieldError adalah kesalahan per field untuk VALIDATION_ERROR.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Error adalah error domain yang aman dikirim ke client.
type Error struct {
	Status int
	Code   string
	Title  string
	Detail string
	Fields []FieldError
	// RetryAfter > 0 → header Retry-After (detik). Dipakai RATE_LIMITED.
	RetryAfter int
	// ExistingResourceID membantu client melanjutkan (contoh ACTIVE_RUN_EXISTS).
	ExistingResourceID string
	// Extension per code (katalog §3): RUN_STALE, APPROVAL_ALREADY_DECIDED, RUN_HAS_ERRORS.
	EmployeesChanged []StaleEmployee
	ChangedCount     *int
	ApprovalStatus   string
	ErrorCount       *int
	Balance          string // INSUFFICIENT_LEAVE_BALANCE: saldo saat ini (desimal string)
	cause            error  // tidak pernah dikirim ke client; hanya untuk log
}

// StaleEmployee: satu karyawan yang membuat run stale (RUN_STALE.employees_changed).
type StaleEmployee struct {
	EmployeeID string `json:"employee_id"`
	EmployeeNo string `json:"employee_no,omitempty"`
	// Reason: INPUT_CHANGED, NEWLY_ELIGIBLE, NO_LONGER_ELIGIBLE.
	Reason string `json:"reason"`
}

// WithStale: RUN_STALE dengan daftar karyawan dan jumlah kandidat perubahan.
func (e *Error) WithStale(employees []StaleEmployee, changed int) *Error {
	c := *e
	c.EmployeesChanged, c.ChangedCount = employees, &changed
	return &c
}

// WithApprovalStatus: APPROVAL_ALREADY_DECIDED menyebut status keputusan yang sudah ada.
func (e *Error) WithApprovalStatus(status string) *Error {
	c := *e
	c.ApprovalStatus = status
	return &c
}

// WithBalance: INSUFFICIENT_LEAVE_BALANCE menyebut saldo saat ini.
func (e *Error) WithBalance(balance string) *Error {
	c := *e
	c.Balance = balance
	return &c
}

// WithErrorCount: RUN_HAS_ERRORS menyebut jumlah item ERROR yang belum di-exclude.
func (e *Error) WithErrorCount(n int) *Error {
	c := *e
	c.ErrorCount = &n
	return &c
}

func (e *Error) Error() string {
	if e.cause != nil {
		return e.Code + ": " + e.cause.Error()
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.cause }

// Is membuat errors.Is(err, apperr.ErrNotFound) cocok berdasarkan Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// WithDetail mengembalikan salinan dengan detail berbeda.
func (e *Error) WithDetail(detail string) *Error {
	c := *e
	c.Detail = detail
	return &c
}

// WithRetryAfter mengembalikan salinan dengan header Retry-After (detik, dibulatkan ke atas, minimal 1).
func (e *Error) WithRetryAfter(seconds int) *Error {
	c := *e
	c.RetryAfter = max(seconds, 1)
	return &c
}

// WithFields mengembalikan salinan dengan daftar field (contoh DUPLICATE_CODE menyebut field `slug`).
func (e *Error) WithFields(fields ...FieldError) *Error {
	c := *e
	c.Fields = fields
	return &c
}

// WithExistingResource mengembalikan salinan yang menyebut ID resource yang sudah ada.
func (e *Error) WithExistingResource(id string) *Error {
	c := *e
	c.ExistingResourceID = id
	return &c
}

// WithCause melampirkan penyebab internal (untuk log), tanpa mengubah apa yang dilihat client.
func (e *Error) WithCause(cause error) *Error {
	c := *e
	c.cause = cause
	return &c
}

func def(status int, code, title string) *Error {
	return &Error{Status: status, Code: code, Title: title}
}

// Generic (katalog §1).
var (
	ErrValidation         = def(http.StatusBadRequest, "VALIDATION_ERROR", "Validation failed")
	ErrMalformedJSON      = def(http.StatusBadRequest, "MALFORMED_JSON", "Malformed JSON body")
	ErrUnauthenticated    = def(http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
	ErrTokenExpired       = def(http.StatusUnauthorized, "TOKEN_EXPIRED", "Access token expired")
	ErrInvalidCredentials = def(http.StatusUnauthorized, "INVALID_CREDENTIALS", "Invalid email or password")
	ErrForbidden          = def(http.StatusForbidden, "FORBIDDEN", "Action not allowed")
	ErrNotFound           = def(http.StatusNotFound, "NOT_FOUND", "Resource not found")
	ErrResourceBusy       = def(http.StatusConflict, "RESOURCE_BUSY", "Resource is busy, retry later")
	ErrEmailRegistered    = def(http.StatusConflict, "EMAIL_ALREADY_REGISTERED", "Email already registered")
	ErrPayloadTooLarge    = def(http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Request body too large")
	ErrUnsupportedMedia   = def(http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Unsupported media type")
	ErrInternal           = def(http.StatusInternalServerError, "INTERNAL_ERROR", "Internal error")
	ErrUnavailable        = def(http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service temporarily unavailable")
	ErrInvalidCursor      = def(http.StatusBadRequest, "INVALID_CURSOR", "Invalid or expired cursor")
	ErrRateLimited        = def(http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests")
	ErrRefreshReused      = def(http.StatusUnauthorized, "REFRESH_TOKEN_REUSED", "Session revoked, please log in again")
	ErrInvalidState       = def(http.StatusConflict, "INVALID_STATE_TRANSITION", "Action not allowed in the current state")
	ErrVersionConflict    = def(http.StatusPreconditionFailed, "VERSION_CONFLICT", "Resource was modified by someone else")
	ErrPreconditionReq    = def(http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", "If-Match header is required")
)

// Idempotency (katalog §1, ADR-L02).
var (
	ErrIdempotencyKeyRequired = def(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required")
	ErrIdempotencyKeyInvalid  = def(http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key header is invalid")
	ErrIdempotencyKeyReused   = def(http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED", "Idempotency-Key was used with a different request")
	ErrRequestInProgress      = def(http.StatusConflict, "REQUEST_IN_PROGRESS", "A request with this Idempotency-Key is still in progress")
)

// Kode khusus PayRun (katalog §3: tenancy, IAM, payroll, approval) ditambahkan bersama fiturnya,
// SETELAH masuk katalog docs/design/15-api-error-catalogue.md (pelajaran LedgerPay §6).
var (
	ErrDuplicateCode            = def(http.StatusConflict, "DUPLICATE_CODE", "Code is already in use")
	ErrEmailVerificationInvalid = def(http.StatusGone, "EMAIL_VERIFICATION_INVALID", "Verification link is invalid or expired")
	// Sprint 2: undangan, employee (katalog §3).
	ErrMembershipExists  = def(http.StatusConflict, "MEMBERSHIP_EXISTS", "User is already a member of this tenant")
	ErrInvitationInvalid = def(http.StatusGone, "INVITATION_INVALID", "Invitation is invalid or expired")
	ErrEmployeeNoTaken   = def(http.StatusConflict, "EMPLOYEE_NO_TAKEN", "Employee number is already in use")
	ErrNIKTaken          = def(http.StatusConflict, "NIK_TAKEN", "NIK is already registered in this tenant")
	ErrNPWPTaken         = def(http.StatusConflict, "NPWP_TAKEN", "NPWP is already registered in this tenant")
	// Sprint 3: kompensasi, effective dating, org (katalog §3).
	ErrOverlappingPeriod  = def(http.StatusConflict, "OVERLAPPING_PERIOD", "Overlaps an existing effective-dated record")
	ErrManagerCycle       = def(http.StatusUnprocessableEntity, "MANAGER_CYCLE", "Manager assignment would create a cycle")
	ErrComponentCycle     = def(http.StatusUnprocessableEntity, "COMPONENT_CYCLE", "Component references form a cycle")
	ErrInvalidComponentRf = def(http.StatusUnprocessableEntity, "INVALID_COMPONENT_REFERENCE", "Invalid component reference")
	// Sprint 4: payroll run, member (katalog §3).
	ErrActiveRunExists = def(http.StatusConflict, "ACTIVE_RUN_EXISTS", "An active payroll run already exists for this period")
	ErrPeriodLocked    = def(http.StatusConflict, "PERIOD_LOCKED", "The payroll period is finalized")
	ErrRunFinalized    = def(http.StatusConflict, "RUN_FINALIZED", "The payroll run is finalized")
	ErrLastOwner       = def(http.StatusConflict, "LAST_OWNER", "The tenant must keep at least one active owner")
	// Sprint 5: approval + stale detection (katalog §3).
	ErrRunStale             = def(http.StatusConflict, "RUN_STALE", "Payroll input changed after calculation; recalculate first")
	ErrApprovalDecided      = def(http.StatusConflict, "APPROVAL_ALREADY_DECIDED", "The approval was already decided or cancelled")
	ErrRunHasErrors         = def(http.StatusUnprocessableEntity, "RUN_HAS_ERRORS", "The run still has ERROR items; fix or exclude them first")
	ErrDecisionNoteRequired = def(http.StatusUnprocessableEntity, "DECISION_NOTE_REQUIRED", "A note is required to reject")
	ErrSelfApprovalReason   = def(http.StatusUnprocessableEntity, "SELF_APPROVAL_REASON_REQUIRED", "No other admin can approve; a self-approval reason is required")
	ErrSelfApproval         = def(http.StatusForbidden, "SELF_APPROVAL_FORBIDDEN", "You cannot approve your own request")
	// Sprint 6: absensi, cuti, lembur (katalog §3).
	ErrOverlappingLeave    = def(http.StatusConflict, "OVERLAPPING_LEAVE", "The dates overlap another pending or approved leave")
	ErrInsufficientBalance = def(http.StatusUnprocessableEntity, "INSUFFICIENT_LEAVE_BALANCE", "Not enough leave balance")
	ErrEmployeeNotActive   = def(http.StatusUnprocessableEntity, "EMPLOYEE_NOT_ACTIVE", "Your account is not linked to an employee active on this date")
	ErrClockOutWithoutIn   = def(http.StatusUnprocessableEntity, "CLOCK_OUT_WITHOUT_CLOCK_IN", "Clock in first")
	ErrNoApproverAvailable = def(http.StatusUnprocessableEntity, "NO_APPROVER_AVAILABLE", "Nobody else in this company can approve the request")
)

// Validation membuat VALIDATION_ERROR dengan daftar field yang salah.
func Validation(fields ...FieldError) *Error {
	e := *ErrValidation
	e.Fields = fields
	return &e
}

// As mengubah error apa pun menjadi *Error. Error yang tidak dikenal menjadi INTERNAL_ERROR
// dengan penyebab asli disimpan untuk log; pesan internal tidak pernah bocor ke client.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return ErrInternal.WithCause(err)
}
