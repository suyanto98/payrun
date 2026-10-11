package engine

import (
	"encoding/json"
	"fmt"
	"time"
)

// Date adalah tanggal lokal tenant tanpa jam (`07-payroll-engine.md` §4: semua "tanggal" di engine adalah tanggal
// lokal; konversi timestamp → tanggal dilakukan InputProvider). Disimpan sebagai tengah malam UTC
// supaya aritmetika hari tidak terpengaruh DST/zona.
type Date struct{ t time.Time }

const dateLayout = "2006-01-02"

// NewDate membuat tanggal dari komponen kalender.
func NewDate(y int, m time.Month, d int) Date { return Date{time.Date(y, m, d, 0, 0, 0, 0, time.UTC)} }

// ParseDate membaca "YYYY-MM-DD".
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return Date{}, fmt.Errorf("engine: invalid date %q", s)
	}
	return Date{t}, nil
}

// MustDate untuk test dan literal.
func MustDate(s string) Date {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func (d Date) String() string               { return d.t.Format(dateLayout) }
func (d Date) IsZero() bool                 { return d.t.IsZero() }
func (d Date) AddDays(n int) Date           { return Date{d.t.AddDate(0, 0, n)} }
func (d Date) Before(o Date) bool           { return d.t.Before(o.t) }
func (d Date) Equal(o Date) bool            { return d.t.Equal(o.t) }
func (d Date) Day() int                     { return d.t.Day() }
func (d Date) Month() time.Month            { return d.t.Month() }
func (d Date) Year() int                    { return d.t.Year() }
func (d Date) Time() time.Time              { return d.t }
func (d Date) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// ISOWeekday: 1 = Senin … 7 = Minggu (sama dengan tenant_settings.work_days).
func (d Date) ISOWeekday() int {
	w := int(d.t.Weekday())
	if w == 0 {
		return 7
	}
	return w
}

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// DateRange adalah rentang half-open [From, To). To nil = tak hingga (sama dengan daterange Postgres).
type DateRange struct {
	From Date  `json:"from"`
	To   *Date `json:"to"`
}

// Contains: d ∈ [From, To).
func (r DateRange) Contains(d Date) bool {
	return !d.Before(r.From) && (r.To == nil || d.Before(*r.To))
}

// Intersect mengembalikan irisan; ok=false kalau kosong.
func (r DateRange) Intersect(o DateRange) (DateRange, bool) {
	from := r.From
	if from.Before(o.From) {
		from = o.From
	}
	to := r.To
	if to == nil || (o.To != nil && o.To.Before(*to)) {
		to = o.To
	}
	if to != nil && !from.Before(*to) {
		return DateRange{}, false
	}
	return DateRange{From: from, To: to}, true
}

// Days memanggil fn untuk setiap tanggal di rentang (To wajib terisi).
func (r DateRange) Days(fn func(Date)) {
	for d := r.From; d.Before(*r.To); d = d.AddDays(1) {
		fn(d)
	}
}

// Last adalah hari terakhir (inklusif) rentang berhingga.
func (r DateRange) Last() Date { return r.To.AddDays(-1) }

func timeMonth(m int) time.Month { return time.Month(m) }
