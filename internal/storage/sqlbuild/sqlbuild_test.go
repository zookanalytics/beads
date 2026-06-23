package sqlbuild

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

func TestOrderByKnownKeys(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sortBy   string
		sortDesc bool
		table    string
		want     string
	}{
		{"", false, "", "ORDER BY priority ASC, created_at DESC, id ASC"},
		{"priority", true, "", "ORDER BY priority DESC, created_at DESC, id ASC"},
		{"created", false, "", "ORDER BY created_at DESC, id ASC"},
		{"created", true, "", "ORDER BY created_at ASC, id ASC"},
		{"title", false, "i", "ORDER BY LOWER(i.title) ASC, i.id ASC"},
		{"updated", false, "i", "ORDER BY i.updated_at DESC, i.id ASC"},
		{"bogus-key", false, "", "ORDER BY priority ASC, created_at DESC, id ASC"},
		{"id", false, "", ""}, // Go-side sort
	}
	for _, tc := range cases {
		if got := OrderBy(tc.sortBy, tc.sortDesc, tc.table); got != tc.want {
			t.Errorf("OrderBy(%q, %v, %q) = %q, want %q", tc.sortBy, tc.sortDesc, tc.table, got, tc.want)
		}
	}
}

// TestUnionSortColumnsCoverSortDefs pins that every SQL-side sort key has a
// sort_* alias in UnionSortColumnsSQL, so UNION consumers can order by any
// key OrderByForColumns may emit.
func TestUnionSortColumnsCoverSortDefs(t *testing.T) {
	t.Parallel()

	for key := range SortDefs {
		alias := "sort_" + key
		if key == "" {
			alias = "sort_priority"
		}
		if !strings.Contains(UnionSortColumnsSQL, alias) {
			t.Errorf("UnionSortColumnsSQL missing alias %q for sort key %q", alias, key)
		}
	}
}

// TestLessMirrorsOrderBy spot-checks that the Go-side comparator agrees with
// the SQL default ordering on the documented tie-break chain: priority ASC,
// then created_at DESC, then id ASC.
func TestLessMirrorsOrderBy(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	older := now.Add(-time.Hour)
	a := &types.Issue{ID: "a", Priority: 1, CreatedAt: now}
	b := &types.Issue{ID: "b", Priority: 2, CreatedAt: now}
	if !Less(a, b, "", false) || Less(b, a, "", false) {
		t.Error("default sort must order priority 1 before priority 2")
	}
	c := &types.Issue{ID: "c", Priority: 1, CreatedAt: older}
	if !Less(a, c, "", false) {
		t.Error("equal priority must order newer created_at first (created_at DESC)")
	}
	d := &types.Issue{ID: "d", Priority: 1, CreatedAt: now}
	if !Less(a, d, "", false) || Less(d, a, "", false) {
		t.Error("full tie must break by id ASC")
	}
}

func TestReadyWorkExcludeTypes(t *testing.T) {
	t.Parallel()

	base := ReadyWorkExcludeTypes(nil)
	seen := make(map[types.IssueType]bool, len(base))
	for _, typ := range base {
		if seen[typ] {
			t.Errorf("duplicate type %q in default exclude list", typ)
		}
		seen[typ] = true
	}
	for _, want := range []types.IssueType{"merge-request", types.TypeGate, types.TypeMolecule, "agent", "rig", "role", "message"} {
		if !seen[want] {
			t.Errorf("default exclude list missing %q", want)
		}
	}

	extended := ReadyWorkExcludeTypes([]types.IssueType{"custom", "", types.TypeGate})
	if got, want := len(extended), len(base)+1; got != want {
		t.Errorf("extras must dedupe and drop empties: len = %d, want %d", got, want)
	}
}

func TestBuildReadyWorkWhereBatchesIDSets(t *testing.T) {
	t.Parallel()

	ids := make([]string, QueryBatchSize+1)
	for i := range ids {
		ids[i] = "x-" + strings.Repeat("a", 3)
	}
	where, args, err := BuildReadyWorkWhere(types.WorkFilter{}, IssuesFilterTables, ReadyWorkWhereInputs{DeferredChildIDs: ids})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Count(where, "id NOT IN ("); got != 2 {
		t.Errorf("expected 2 batched NOT IN clauses for %d IDs, got %d", len(ids), got)
	}
	wantArgs := len(ids) + len(ReadyWorkExcludeTypes(nil))
	if len(args) != wantArgs {
		t.Errorf("args = %d, want %d", len(args), wantArgs)
	}
}

// countsAggregateAliases are the columns the SearchCountsSQL mega-query
// projects from its OUTER scope. They are not in scope inside the pre-join
// derived subquery that the WHERE filter renders into, so no WHERE clause may
// reference them. See the SearchCountsSQL doc comment.
var countsAggregateAliases = []string{
	"labels_json",
	"dep_count",
	"rdep_count",
	"comment_count",
	"parent_id",
	"deps_json",
}

// TestWhereClausesNeverReferenceAggregates enforces the load-bearing invariant
// behind the filter-before-join shape: the WHERE producers (BuildIssueFilterClauses
// and BuildReadyWorkWhere) must only ever reference issue columns, never the
// counts aggregate aliases. If you add a filter field, populate it below so its
// clause is exercised here; a count-driven predicate must go through a separate
// outer predicate, not these builders.
func TestWhereClausesNeverReferenceAggregates(t *testing.T) {
	t.Parallel()

	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	tm := func() *time.Time { v := time.Unix(0, 0).UTC(); return &v }
	status := func(s types.Status) *types.Status { return &s }
	itype := func(s types.IssueType) *types.IssueType { return &s }
	b := func(v bool) *bool { return &v }

	// Maximally-populated IssueFilter: every field that contributes a WHERE clause.
	issueFilter := types.IssueFilter{
		Status:              status(types.StatusOpen),
		Statuses:            []types.Status{types.StatusOpen, types.StatusInProgress},
		ExcludeStatus:       []types.Status{types.StatusClosed},
		Priority:            num(1),
		PriorityMin:         num(0),
		PriorityMax:         num(3),
		IssueType:           itype(types.TypeTask),
		ExcludeTypes:        []types.IssueType{types.TypeTask},
		Assignee:            str("alice"),
		Labels:              []string{"a", "b"},
		LabelsAny:           []string{"c", "d"},
		ExcludeLabels:       []string{"e"},
		NoLabels:            true,
		TitleSearch:         "ts",
		TitleContains:       "tc",
		DescriptionContains: "dc",
		NotesContains:       "nc",
		ExternalRefContains: "erc",
		IDs:                 []string{"bd-1", "bd-2"},
		IDPrefix:            "bd-",
		SpecIDPrefix:        "spec-",
		EmptyDescription:    true,
		NoAssignee:          true,
		SourceRepo:          str("repo"),
		Ephemeral:           b(true),
		Pinned:              b(false),
		IsTemplate:          b(true),
		ParentID:            str("bd-parent"),
		NoParent:            true,
		MolType:             func() *types.MolType { v := types.MolType("work"); return &v }(),
		WispType:            func() *types.WispType { v := types.WispType("ping"); return &v }(),
		CreatedAfter:        tm(), CreatedBefore: tm(),
		UpdatedAfter: tm(), UpdatedBefore: tm(),
		ClosedAfter: tm(), ClosedBefore: tm(),
		StartedAfter: tm(), StartedBefore: tm(),
		DeferAfter: tm(), DeferBefore: tm(),
		DueAfter: tm(), DueBefore: tm(),
		Deferred:       true,
		Overdue:        true,
		MetadataFields: map[string]string{"k": "v"},
		HasMetadataKey: "hk",
	}

	// Maximally-populated WorkFilter for the ready-work builder.
	workFilter := types.WorkFilter{
		Status:         types.StatusOpen,
		Type:           "task",
		Priority:       num(1),
		Assignee:       str("bob"),
		Unassigned:     false,
		Labels:         []string{"a"},
		ExcludeLabels:  []string{"b"},
		ExcludeTypes:   []types.IssueType{types.TypeTask},
		ParentID:       str("bd-parent"),
		MoleculeID:     "bd-mol",
		MetadataFields: map[string]string{"k": "v"},
		HasMetadataKey: "hk",
	}
	readyInputs := ReadyWorkWhereInputs{
		DeferredChildIDs:    []string{"bd-d1", "bd-d2"},
		ParentDescendantIDs: []string{"bd-c1", "bd-c2"},
	}

	for _, tables := range []FilterTables{IssuesFilterTables, WispsFilterTables} {
		var rendered []string

		// Both query branches (ID-like and free-text) of BuildIssueFilterClauses.
		for _, q := range []string{"bd-123", "free text"} {
			clauses, _, err := BuildIssueFilterClauses(q, issueFilter, tables)
			if err != nil {
				t.Fatalf("BuildIssueFilterClauses(%q): %v", q, err)
			}
			rendered = append(rendered, clauses...)
		}

		readyWhere, _, err := BuildReadyWorkWhere(workFilter, tables, readyInputs)
		if err != nil {
			t.Fatalf("BuildReadyWorkWhere: %v", err)
		}
		readyUnassigned := workFilter
		readyUnassigned.Unassigned = true
		readyUnassignedWhere, _, err := BuildReadyWorkWhere(readyUnassigned, tables, readyInputs)
		if err != nil {
			t.Fatalf("BuildReadyWorkWhere (unassigned): %v", err)
		}
		rendered = append(rendered, readyWhere, readyUnassignedWhere)

		for _, clause := range rendered {
			for _, alias := range countsAggregateAliases {
				if strings.Contains(clause, alias) {
					t.Errorf("WHERE clause references counts aggregate alias %q (out of scope in the pre-join subquery): %s", alias, clause)
				}
			}
		}
	}
}

func TestSearchCountsSQLShape(t *testing.T) {
	t.Parallel()

	sql := SearchCountsSQL(WispsFilterTables, "WHERE x = ?", "ORDER BY y", "LIMIT 5", true, false)
	for _, want := range []string{
		"FROM wisps i",
		"FROM wisp_dependencies",
		"FROM wisp_comments",
		"FROM wisp_labels",
		"UNION ALL", // wisp reverse deps included
		"WHERE x = ?",
		"ORDER BY y",
		"LIMIT 5",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("counts SQL missing %q", want)
		}
	}

	// Filter-before-join structure: the WHERE filter is applied to the main
	// table in an inner subquery that closes before the first aggregate LEFT
	// JOIN. Locking this prevents a regression back to the filter-after-join
	// shape that materialized every aggregate before pruning.
	subqEnd := strings.Index(sql, ") i")
	firstJoin := strings.Index(sql, "LEFT JOIN")
	if !strings.Contains(sql, "FROM (") || subqEnd < 0 {
		t.Fatalf("counts SQL must wrap the main table in a derived subquery; got:\n%s", sql)
	}
	if firstJoin < 0 || subqEnd > firstJoin {
		t.Fatalf("inner subquery must close before the first aggregate LEFT JOIN")
	}
	if idx := strings.Index(sql, "WHERE x = ?"); idx < 0 || idx > subqEnd {
		t.Errorf("WHERE filter must appear inside the inner subquery (before %q)", ") i")
	}
	// ORDER BY and LIMIT must stay AFTER the joins, and appear exactly once —
	// the ready-work path passes a parameterized ORDER BY, so duplicating it
	// would desync the placeholder/arg counts.
	for _, outer := range []string{"ORDER BY y", "LIMIT 5"} {
		if strings.Count(sql, outer) != 1 {
			t.Errorf("%q must appear exactly once, got %d", outer, strings.Count(sql, outer))
		}
		if idx := strings.Index(sql, outer); idx < firstJoin {
			t.Errorf("%q must appear after the aggregate joins", outer)
		}
	}

	noWispDeps := SearchCountsSQL(IssuesFilterTables, "", "", "", false, true)
	if strings.Contains(noWispDeps, "UNION ALL") {
		t.Error("counts SQL must not union wisp reverse deps when probe says absent")
	}
	if strings.Contains(noWispDeps, "JSON_ARRAYAGG(label)") {
		t.Error("counts SQL must skip the labels join when skipLabels is set")
	}
	if !strings.Contains(noWispDeps, "NULL AS labels_json") {
		t.Error("counts SQL must project NULL labels_json when skipLabels is set")
	}
}
