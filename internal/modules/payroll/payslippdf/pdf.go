// Package payslippdf merender slip gaji PDF (PF-22) secara murni dan deterministik: input sama → byte
// sama (PFS-10, PI-12). Tidak ada waktu dinding: tanggal dokumen = finalized_at run. Font standar
// Helvetica (tanpa embed), teks di-transliterasi ke cp1252.
package payslippdf

import (
	"bytes"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/shopspring/decimal"
)

type Line struct {
	Name        string
	Earning     bool
	Amount      decimal.Decimal
	Explanation string
}

type Slip struct {
	TenantName   string
	Period       string // YYYY-MM
	EmployeeNo   string
	EmployeeName string
	Position     string
	Department   string
	BankMasked   string // contoh "BCA ••••1234"; kosong = tidak ada
	Lines        []Line
	Gross        decimal.Decimal
	Deductions   decimal.Decimal
	Net          decimal.Decimal
	Ruleset      string
	FinalizedAt  time.Time
}

var months = [...]string{"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}

// PeriodLabel: "2026-03" → "Maret 2026".
func PeriodLabel(p string) string {
	t, err := time.Parse("2006-01", p)
	if err != nil {
		return p
	}
	return months[t.Month()-1] + " " + t.Format("2006")
}

// Rupiah: 1234567.5 → "Rp 1.234.568" (slip menampilkan rupiah bulat; nilai engine sudah dibulatkan).
func Rupiah(d decimal.Decimal) string {
	neg := d.IsNegative()
	s := d.Abs().Round(0).StringFixed(0)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-Rp " + b.String()
	}
	return "Rp " + b.String()
}

// Render menghasilkan PDF A4. Error hanya dari fpdf (input tidak valid untuk font).
func Render(s Slip) ([]byte, error) {
	p := fpdf.New("P", "mm", "A4", "")
	p.SetCatalogSort(true)
	at := s.FinalizedAt.UTC()
	p.SetCreationDate(at)
	p.SetModificationDate(at)
	p.SetCompression(true)
	p.SetTitle("Slip gaji "+s.Period+" "+s.EmployeeNo, true)
	p.SetCreator("PayRun", true)
	p.SetMargins(18, 18, 18)
	p.SetAutoPageBreak(true, 18)
	tr := p.UnicodeTranslatorFromDescriptor("")
	p.AddPage()
	w, _ := p.GetPageSize()
	inner := w - 36

	p.SetFont("Helvetica", "B", 16)
	p.CellFormat(inner, 8, tr(s.TenantName), "", 1, "L", false, 0, "")
	p.SetFont("Helvetica", "", 11)
	p.CellFormat(inner, 6, tr("Slip gaji periode "+PeriodLabel(s.Period)), "", 1, "L", false, 0, "")
	p.Ln(4)

	kv := func(k, v string) {
		p.SetFont("Helvetica", "", 10)
		p.CellFormat(40, 6, tr(k), "", 0, "L", false, 0, "")
		p.SetFont("Helvetica", "B", 10)
		p.CellFormat(inner-40, 6, tr(v), "", 1, "L", false, 0, "")
	}
	kv("Nama", s.EmployeeName)
	kv("No. karyawan", s.EmployeeNo)
	if s.Position != "" || s.Department != "" {
		kv("Jabatan", strings.Trim(s.Position+" · "+s.Department, " ·"))
	}
	if s.BankMasked != "" {
		kv("Rekening", s.BankMasked)
	}
	p.Ln(4)

	section := func(title string, earning bool) {
		p.SetFont("Helvetica", "B", 11)
		p.SetFillColor(240, 240, 240)
		p.CellFormat(inner, 7, tr(title), "", 1, "L", true, 0, "")
		for _, l := range s.Lines {
			if l.Earning != earning {
				continue
			}
			p.SetFont("Helvetica", "", 10)
			p.CellFormat(inner-45, 6, tr(l.Name), "", 0, "L", false, 0, "")
			p.CellFormat(45, 6, tr(Rupiah(l.Amount)), "", 1, "R", false, 0, "")
			if l.Explanation != "" {
				p.SetFont("Helvetica", "", 8)
				p.SetTextColor(100, 100, 100)
				p.MultiCell(inner-45, 4, tr(l.Explanation), "", "L", false)
				p.SetTextColor(0, 0, 0)
			}
		}
		p.Ln(2)
	}
	section("Pendapatan", true)
	section("Potongan", false)

	total := func(k string, v decimal.Decimal, bold bool) {
		style := ""
		if bold {
			style = "B"
		}
		p.SetFont("Helvetica", style, 11)
		p.CellFormat(inner-45, 7, tr(k), "T", 0, "L", false, 0, "")
		p.CellFormat(45, 7, tr(Rupiah(v)), "T", 1, "R", false, 0, "")
	}
	total("Total pendapatan", s.Gross, false)
	total("Total potongan", s.Deductions, false)
	total("Gaji bersih (take home pay)", s.Net, true)
	p.Ln(6)
	p.SetFont("Helvetica", "", 8)
	p.SetTextColor(100, 100, 100)
	p.MultiCell(inner, 4, tr("Ruleset "+s.Ruleset+" · difinalisasi "+at.Format("2006-01-02 15:04")+
		" UTC · dokumen dibuat otomatis, sah tanpa tanda tangan."), "", "L", false)

	var b bytes.Buffer
	if err := p.Output(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
