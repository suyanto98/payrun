// Package org: master data organisasi (PF-05): departments dan positions. Keduanya berbentuk sama
// (code unik per tenant, name, status ACTIVE/INACTIVE), sehingga satu service melayani dua "kind"
// dengan tabel query masing-masing. Penugasan karyawan (org_assignments, effective-dated) masuk S3.
package org

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/modules/org/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/db"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

// Unit adalah satu departemen atau posisi (respons G1–G4).
type Unit struct {
	ID      uuid.UUID `json:"id"`
	Code    string    `json:"code"`
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Version int32     `json:"version"`
}

// row = bentuk baris semua query org (sqlc membuat tipe per query dengan field identik; Go
// mengizinkan konversi antar struct yang field-nya sama).
type row = store.GetDepartmentRow

// Kind memilih tabel. Nama dipakai sebagai target_type audit dan prefix action.
type Kind struct {
	Name       string // "department" / "position"
	table      string
	list       func(q *store.Queries, ctx context.Context, status string) ([]row, error)
	get        func(q *store.Queries, ctx context.Context, id uuid.UUID) (row, error)
	create     func(q *store.Queries, ctx context.Context, tid, id uuid.UUID, code, name string, actor uuid.UUID) (row, error)
	update     func(q *store.Queries, ctx context.Context, id uuid.UUID, version int32, name string, actor uuid.UUID) (row, error)
	deactivate func(q *store.Queries, ctx context.Context, id, actor uuid.UUID) (row, error)
}

var Departments = Kind{
	Name: "department", table: "departments",
	list: func(q *store.Queries, ctx context.Context, status string) ([]row, error) {
		rs, err := q.ListDepartments(ctx, status)
		out := make([]row, len(rs))
		for i, r := range rs {
			out[i] = row(r)
		}
		return out, err
	},
	get: func(q *store.Queries, ctx context.Context, id uuid.UUID) (row, error) {
		return q.GetDepartment(ctx, id)
	},
	create: func(q *store.Queries, ctx context.Context, tid, id uuid.UUID, code, name string, actor uuid.UUID) (row, error) {
		r, err := q.CreateDepartment(ctx, store.CreateDepartmentParams{TenantID: tid, ID: id, Code: code, Name: name, Actor: &actor})
		return row(r), err
	},
	update: func(q *store.Queries, ctx context.Context, id uuid.UUID, version int32, name string, actor uuid.UUID) (row, error) {
		r, err := q.UpdateDepartment(ctx, store.UpdateDepartmentParams{ID: id, Version: version, Name: name, Actor: &actor})
		return row(r), err
	},
	deactivate: func(q *store.Queries, ctx context.Context, id, actor uuid.UUID) (row, error) {
		r, err := q.DeactivateDepartment(ctx, store.DeactivateDepartmentParams{ID: id, Actor: &actor})
		return row(r), err
	},
}

var Positions = Kind{
	Name: "position", table: "positions",
	list: func(q *store.Queries, ctx context.Context, status string) ([]row, error) {
		rs, err := q.ListPositions(ctx, status)
		out := make([]row, len(rs))
		for i, r := range rs {
			out[i] = row(r)
		}
		return out, err
	},
	get: func(q *store.Queries, ctx context.Context, id uuid.UUID) (row, error) {
		r, err := q.GetPosition(ctx, id)
		return row(r), err
	},
	create: func(q *store.Queries, ctx context.Context, tid, id uuid.UUID, code, name string, actor uuid.UUID) (row, error) {
		r, err := q.CreatePosition(ctx, store.CreatePositionParams{TenantID: tid, ID: id, Code: code, Name: name, Actor: &actor})
		return row(r), err
	},
	update: func(q *store.Queries, ctx context.Context, id uuid.UUID, version int32, name string, actor uuid.UUID) (row, error) {
		r, err := q.UpdatePosition(ctx, store.UpdatePositionParams{ID: id, Version: version, Name: name, Actor: &actor})
		return row(r), err
	},
	deactivate: func(q *store.Queries, ctx context.Context, id, actor uuid.UUID) (row, error) {
		r, err := q.DeactivatePosition(ctx, store.DeactivatePositionParams{ID: id, Actor: &actor})
		return row(r), err
	},
}

type Service struct {
	pool *pgxpool.Pool
	ids  idgen.Generator
}

func NewService(pool *pgxpool.Pool, ids idgen.Generator) *Service {
	return &Service{pool: pool, ids: ids}
}

// toUnit men-decode baris dengan tenant guard C-46.
func toUnit(ctx context.Context, k Kind, r row) (Unit, error) {
	if err := db.GuardTenant(ctx, k.table, r.TenantID); err != nil {
		return Unit{}, err
	}
	return Unit{ID: r.ID, Code: r.Code, Name: r.Name, Status: r.Status, Version: r.Version}, nil
}

func (s *Service) List(ctx context.Context, k Kind, status string) ([]Unit, error) {
	if status == "" {
		status = "ACTIVE"
	}
	if status != "ACTIVE" && status != "INACTIVE" {
		return nil, apperr.Validation(apperr.FieldError{Field: "status", Code: "INVALID_ENUM", Message: "must be ACTIVE or INACTIVE"})
	}
	out := []Unit{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		rs, err := k.list(store.New(tx), ctx, status)
		if err != nil {
			return err
		}
		for _, r := range rs {
			u, err := toUnit(ctx, k, r)
			if err != nil {
				return err
			}
			out = append(out, u)
		}
		return nil
	})
	return out, db.MapError(err)
}

var codePattern = regexp.MustCompile(`^[A-Z0-9_]{1,20}$`)

func validName(name string) *apperr.FieldError {
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return &apperr.FieldError{Field: "name", Code: "LENGTH", Message: "must be 1..100 characters"}
	}
	return nil
}

func (s *Service) Create(ctx context.Context, k Kind, actor uuid.UUID, code, name string, meta reqmeta.Meta) (Unit, error) {
	code, name = strings.TrimSpace(code), strings.TrimSpace(name)
	var fe []apperr.FieldError
	if !codePattern.MatchString(code) {
		fe = append(fe, apperr.FieldError{Field: "code", Code: "FORMAT", Message: "must match ^[A-Z0-9_]{1,20}$"})
	}
	if e := validName(name); e != nil {
		fe = append(fe, *e)
	}
	if len(fe) > 0 {
		return Unit{}, apperr.Validation(fe...)
	}
	tid, _ := tenantctx.From(ctx)
	var u Unit
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := k.create(store.New(tx), ctx, tid, s.ids.New(), code, name, actor)
		if err != nil {
			return err
		}
		if u, err = toUnit(ctx, k, r); err != nil {
			return err
		}
		return s.audit(ctx, tx, k, actor, "created", u.ID, map[string]any{"code": code, "name": name}, meta)
	})
	return u, db.MapError(err)
}

// Update mengganti name (code immutable). version = If-Match.
func (s *Service) Update(ctx context.Context, k Kind, actor, id uuid.UUID, version int32, name string, meta reqmeta.Meta) (Unit, error) {
	name = strings.TrimSpace(name)
	if e := validName(name); e != nil {
		return Unit{}, apperr.Validation(*e)
	}
	var u Unit
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		r, err := k.update(q, ctx, id, version, name, actor)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := k.get(q, ctx, id); gerr != nil {
				return gerr // tidak ada (atau tenant lain) → 404
			}
			return apperr.ErrVersionConflict
		}
		if err != nil {
			return err
		}
		if u, err = toUnit(ctx, k, r); err != nil {
			return err
		}
		return s.audit(ctx, tx, k, actor, "updated", id, map[string]any{"name": name}, meta)
	})
	return u, db.MapError(err)
}

// Deactivate: ACTIVE → INACTIVE; yang sudah INACTIVE dikembalikan apa adanya (natural idempotent).
// Cek "masih dipakai org_assignments hari ini" (409) masuk S3 bersama tabelnya.
func (s *Service) Deactivate(ctx context.Context, k Kind, actor, id uuid.UUID, meta reqmeta.Meta) (Unit, error) {
	var u Unit
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		r, err := k.deactivate(q, ctx, id, actor)
		if errors.Is(err, pgx.ErrNoRows) {
			cur, gerr := k.get(q, ctx, id)
			if gerr != nil {
				return gerr
			}
			u, err = toUnit(ctx, k, cur)
			return err
		}
		if err != nil {
			return err
		}
		if u, err = toUnit(ctx, k, r); err != nil {
			return err
		}
		return s.audit(ctx, tx, k, actor, "deactivated", id, nil, meta)
	})
	return u, db.MapError(err)
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, k Kind, actor uuid.UUID, verb string, id uuid.UUID, diff map[string]any, meta reqmeta.Meta) error {
	tid, _ := tenantctx.From(ctx)
	return audit.Record(ctx, tx, s.ids.New(), audit.Entry{
		TenantID: tid, ActorID: actor, Action: k.Name + "." + verb, TargetType: k.Name, TargetID: id, Diff: diff,
	}, meta)
}
