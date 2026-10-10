package engine

import (
	"sort"
	"strings"
)

// Validasi graf referensi PERCENT_OF (§2.4). Dipakai dua kali: saat konfigurasi disimpan (modul
// compensation → 422) dan defensif saat kalkulasi (→ item ERROR). Fungsi murni, tanpa DB.

// GraphProblem menjelaskan masalah konfigurasi. Code = COMPONENT_CYCLE atau INVALID_COMPONENT_REFERENCE
// (nama code API; engine memetakan yang kedua ke COMPONENT_REF_MISSING untuk item).
type GraphProblem struct {
	Code      string
	Component string
	Detail    string
	Cycle     []string
}

const (
	ProblemCycle  = "COMPONENT_CYCLE"
	ProblemBadRef = "INVALID_COMPONENT_REFERENCE"
)

func isVirtualBase(ref string) bool {
	switch ref {
	case BaseSalaryRef, FixedAllowancesRef, BPJSWageRef, TaxableGrossRef:
		return true
	}
	return false
}

// virtualBaseStage: stage paling awal tempat base virtual sudah lengkap. Komponen yang mereferensikannya
// harus berada di stage yang SAMA ATAU LEBIH AKHIR.
var virtualBaseStage = map[string]Stage{
	BaseSalaryRef: StageEarning, FixedAllowancesRef: StageEarning, BPJSWageRef: StageStatutory, TaxableGrossRef: StageTax,
}

// ValidateGraph memeriksa semua definisi AKTIF: referensi ke komponen yang ada dan aktif, tidak ke
// stage yang lebih akhir, dan tanpa siklus. Mengembalikan urutan evaluasi topologis per code
// (deterministik: dependensi lebih dulu, lalu urut code) bila valid.
func ValidateGraph(defs []ComponentDef) ([]string, *GraphProblem) {
	byCode := map[string]ComponentDef{}
	for _, d := range defs {
		if d.Active {
			byCode[d.Code] = d
		}
	}
	codes := make([]string, 0, len(byCode))
	for c := range byCode {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		d := byCode[c]
		if d.CalcType != PercentOf || d.BaseRef == "" {
			continue
		}
		if isVirtualBase(d.BaseRef) {
			if stageOrder[virtualBaseStage[d.BaseRef]] > stageOrder[d.Stage] {
				return nil, &GraphProblem{Code: ProblemBadRef, Component: c, Detail: d.BaseRef + " is only complete in a later stage"}
			}
			continue
		}
		ref, ok := byCode[d.BaseRef]
		if !ok {
			return nil, &GraphProblem{Code: ProblemBadRef, Component: c, Detail: "base_ref " + d.BaseRef + " does not exist or is inactive"}
		}
		if stageOrder[ref.Stage] > stageOrder[d.Stage] {
			return nil, &GraphProblem{Code: ProblemBadRef, Component: c, Detail: "base_ref " + d.BaseRef + " is in a later stage"}
		}
	}
	// Topological sort (DFS) dengan deteksi siklus; hanya edge PERCENT_OF → komponen.
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var order []string
	var stack []string
	var visit func(c string) *GraphProblem
	visit = func(c string) *GraphProblem {
		switch color[c] {
		case grey:
			i := len(stack) - 1
			for i >= 0 && stack[i] != c {
				i--
			}
			cycle := append(append([]string{}, stack[i:]...), c)
			return &GraphProblem{Code: ProblemCycle, Component: c, Detail: strings.Join(cycle, " → "), Cycle: cycle}
		case black:
			return nil
		}
		color[c] = grey
		stack = append(stack, c)
		d := byCode[c]
		if d.CalcType == PercentOf && d.BaseRef != "" && !isVirtualBase(d.BaseRef) {
			if p := visit(d.BaseRef); p != nil {
				return p
			}
		}
		stack = stack[:len(stack)-1]
		color[c] = black
		order = append(order, c)
		return nil
	}
	for _, c := range codes {
		if p := visit(c); p != nil {
			return nil, p
		}
	}
	return order, nil
}
