package contract

import (
	"fmt"
	"sort"

	"codemap/store"
)

// DetectDrift compares two structs and produces a drift report.
func DetectDrift(fromRef, toRef string, a, b StructInfo, now string) *store.DriftReport {
	comp := CompareStructShapes(a, b)

	if comp.Identical {
		return nil
	}

	severity := ClassifyDriftSeverity(comp)
	fields := buildDriftFields(comp, a.Repo, b.Repo)

	return &store.DriftReport{
		FromRef:   fromRef,
		ToRef:     toRef,
		Severity:  severity,
		Fields:    fields,
		IndexedAt: now,
	}
}

// buildDriftFields converts shape comparison results to drift field entries.
// When fields appear on both sides but under different wire names (a rename),
// they are paired by type and reported as a single "renamed" entry.
func buildDriftFields(comp ShapeComparison, repoA, repoB string) []store.DriftField {
	if len(comp.OnlyInA) > 0 && len(comp.OnlyInB) > 0 {
		return buildRenamedFields(comp.OnlyInA, comp.OnlyInB, repoA, repoB)
	}

	var fields []store.DriftField

	// Fields only in A (removed from consumer's perspective).
	for _, f := range comp.OnlyInA {
		fields = append(fields, store.DriftField{
			WireName: f.WireName,
			TypeA:    f.GoType,
			TypeB:    "",
			RepoA:    repoA,
			RepoB:    repoB,
			Status:   "removed",
		})
	}

	// Fields only in B (added in consumer's perspective).
	for _, f := range comp.OnlyInB {
		fields = append(fields, store.DriftField{
			WireName: f.WireName,
			TypeA:    "",
			TypeB:    f.GoType,
			RepoA:    repoA,
			RepoB:    repoB,
			Status:   "added",
		})
	}

	// Type changes on shared fields.
	for _, d := range comp.Divergences {
		fields = append(fields, store.DriftField{
			WireName: d.WireName,
			TypeA:    d.TypeA,
			TypeB:    d.TypeB,
			RepoA:    repoA,
			RepoB:    repoB,
			Status:   d.Status,
		})
	}

	return fields
}

// buildRenamedFields pairs fields of the same type that moved wire names, the
// strongest signal of a rename (design edge case #5).
func buildRenamedFields(onlyA, onlyB []FieldInfo, repoA, repoB string) []store.DriftField {
	pairedB := make(map[int]bool)
	var fields []store.DriftField
	for _, a := range onlyA {
		for bi, b := range onlyB {
			if pairedB[bi] || a.GoType != b.GoType {
				continue
			}
			pairedB[bi] = true
			fields = append(fields, store.DriftField{
				WireName: a.WireName + " → " + b.WireName,
				TypeA:    a.GoType,
				TypeB:    b.GoType,
				RepoA:    repoA,
				RepoB:    repoB,
				Status:   "renamed",
			})
			break
		}
	}
	return fields
}

// DetectConstantDrift compares two constants by their symbolic (short) name:
// same name with different values is breaking value drift; different names
// sharing a value is noted as evidence (design edge case #10).
func DetectConstantDrift(a, b ConstantRef, now string) *store.DriftReport {
	switch {
	case a.Value != b.Value && extractShortName(a.Name) == extractShortName(b.Name):
		return &store.DriftReport{
			FromRef:  a.Name,
			ToRef:    b.Name,
			Severity: store.DriftSeverityBreaking,
			Fields: []store.DriftField{
				{
					WireName: "constant_value",
					TypeA:    a.Value,
					TypeB:    b.Value,
					RepoA:    a.Repo,
					RepoB:    b.Repo,
					Status:   "value_changed",
				},
			},
			IndexedAt: now,
		}
	case a.Value == b.Value && extractShortName(a.Name) != extractShortName(b.Name):
		return &store.DriftReport{
			FromRef:  a.Name,
			ToRef:    b.Name,
			Severity: store.DriftSeverityUnknown,
			Fields: []store.DriftField{
				{
					WireName: "constant_value",
					TypeA:    a.Value,
					TypeB:    b.Value,
					RepoA:    a.Repo,
					RepoB:    b.Repo,
					Status:   "same_value_different_name",
				},
			},
			IndexedAt: now,
		}
	default:
		return nil
	}
}

// FindConstantDrift detects constant-level drift across repos.
func FindConstantDrift(analysis *Analysis, now string) []store.DriftReport {
	nameIndex := buildNameIndex(analysis)
	valueIndex := buildReverseValueIndex(analysis)

	total := 0
	for _, refs := range nameIndex {
		total += len(refs)
	}
	reports := make([]store.DriftReport, 0, total)
	reports = append(reports, findValueDrifts(nameIndex, now)...)
	reports = append(reports, findNameDrifts(valueIndex, now)...)
	return reports
}

func buildNameIndex(analysis *Analysis) map[string][]ConstantRef {
	nameIndex := make(map[string][]ConstantRef)
	for repo, consts := range analysis.ConstantsByRepo {
		for name, value := range consts {
			short := extractShortName(name)
			nameIndex[short] = append(nameIndex[short], ConstantRef{Repo: repo, Name: name, Value: value})
		}
	}
	return nameIndex
}

func buildReverseValueIndex(analysis *Analysis) map[string][]ConstantRef {
	valueIndex := make(map[string][]ConstantRef)
	for repo, consts := range analysis.ConstantsByRepo {
		for name, value := range consts {
			valueIndex[value] = append(valueIndex[value], ConstantRef{Repo: repo, Name: name, Value: value})
		}
	}
	return valueIndex
}

func findValueDrifts(nameIndex map[string][]ConstantRef, now string) []store.DriftReport {
	var reports []store.DriftReport
	for _, refs := range nameIndex {
		if len(refs) < 2 {
			continue
		}
		sort.Slice(refs, func(i, j int) bool { return refLess(refs[i], refs[j]) })
		for i := range refs {
			for j := i + 1; j < len(refs); j++ {
				if refs[i].Repo == refs[j].Repo || refs[i].Value == refs[j].Value {
					continue
				}
				d := DetectConstantDrift(refs[i], refs[j], now)
				if d != nil {
					d.Evidence = formatValueDriftEvidence(refs[i], refs[j])
					reports = append(reports, *d)
				}
			}
		}
	}
	return reports
}

func findNameDrifts(valueIndex map[string][]ConstantRef, now string) []store.DriftReport {
	var reports []store.DriftReport
	for _, refs := range valueIndex {
		if len(refs) < 2 {
			continue
		}
		sort.Slice(refs, func(i, j int) bool { return refLess(refs[i], refs[j]) })
		for i := range refs {
			for j := i + 1; j < len(refs); j++ {
				// Same symbolic name sharing a value is a true match, not drift.
				if refs[i].Repo == refs[j].Repo || extractShortName(refs[i].Name) == extractShortName(refs[j].Name) {
					continue
				}
				d := DetectConstantDrift(refs[i], refs[j], now)
				if d != nil {
					d.Evidence = formatNameDriftEvidence(refs[i], refs[j])
					reports = append(reports, *d)
				}
			}
		}
	}
	return reports
}

// refLess canonicalizes constant ref ordering for deterministic drift reports.
func refLess(a, b ConstantRef) bool {
	if a.Repo != b.Repo {
		return a.Repo < b.Repo
	}
	return a.Name < b.Name
}

func formatValueDriftEvidence(a, b ConstantRef) string {
	return fmt.Sprintf("constant %q: value %q in %s vs %q in %s",
		a.Name, a.Value, a.Repo, b.Value, b.Repo)
}

func formatNameDriftEvidence(a, b ConstantRef) string {
	return fmt.Sprintf("constants %q and %q share value %q across repos %s and %s",
		a.Name, b.Name, a.Value, a.Repo, b.Repo)
}
