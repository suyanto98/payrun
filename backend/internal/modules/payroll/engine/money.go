package engine

import (
	"strings"

	"github.com/shopspring/decimal"
)

// Presisi pembagian hasil antara (ADR-P06: ≥ 16 digit). Hasil antara TIDAK dibulatkan; pembulatan
// hanya sekali per baris, tepat sebelum baris ditambahkan (§3).
const divPrecision = 20

var (
	zero    = decimal.Zero
	hundred = decimal.NewFromInt(100)
)

func div(a, b decimal.Decimal) decimal.Decimal { return a.DivRound(b, divPrecision) }

// round membulatkan ke rupiah (0 desimal) dengan mode baris. Nilai uang di engine selalu ≥ 0
// (tanda ada di Category), sehingga DOWN = ke bawah dan UP = ke atas.
func round(v decimal.Decimal, mode Rounding) decimal.Decimal {
	switch mode {
	case Down:
		return v.RoundDown(0)
	case Up:
		return v.RoundUp(0)
	default:
		return v.Round(0) // half away from zero = half-up untuk nilai positif
	}
}

// rupiah: "Rp6.600.000" atau "Rp38.150,29" (format Indonesia, untuk explanation).
func rupiah(v decimal.Decimal, decimals int32) string {
	s := v.StringFixed(decimals)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	out := "Rp" + b.String()
	if frac != "" {
		out += "," + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// num: angka tanpa nol berlebih dengan koma desimal ("1,5", "11", "0,015").
func num(v decimal.Decimal) string { return strings.ReplaceAll(v.String(), ".", ",") }

// percent: 0.02 → "2%", 0.015 → "1,5%".
func percent(rate decimal.Decimal) string { return num(rate.Mul(hundred)) + "%" }
