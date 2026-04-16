# PLAN: Fix conceptSpaces label variable-ordering divergence (xtrace 406841)

Created: 2026-04-16 (afternoon, fifth pass)

## Context

After the previous two fixes (plan 312 nil-vs-empty-list `decide()`, and the `NamedSpace.sexp/canon` monkey-patch), the user re-ran `make tlb` and the trace now matches all the way to the `conceptSpaces` field of `module.CanonSnapshot`. The test reports a new divergence at xtrace event **406841** (`/Users/jaten/ivy/goivy/log.tlb`).

The diff (`go '-'` vs `py '+'`) is ONLY in the `terms:` list of certain conjecture **labels** inside `conceptSpaces=[...]`. The bodies match exactly — same variables, same sorts, same nesting — but the labels emit the variable list in a different order:

```
conjecture:1   (2 vars)  Go:[P1 P2]      Python:[P2 P1]
conjecture:11  (2 vars)  Go:[C  P]       Python:[P  C]
conjecture:?   (3 vars)  Go:[P1 C  P2]   Python:[P2 P1 C]
conjecture:12  (truncated diff)         Go:C          Python:P
```

The function-symbol sorts are also affected (RelationSort is built from the same variable order), but in tlb.ivy all label variables happen to be `tlb.processor`, so the `(FunctionSort sorts:[...])` text is identical for both sides; only the `Variable name:` text in the `terms:` list differs.

### Root cause: nondeterministic set/map iteration on both sides

Both languages collect "free variables in the formula" into an unordered container, then list it.

**Python** (`~/ivy/pyivy/ivy/ivy/ivy_module.py:272-281`, `update_conjs`):
```python
def update_conjs(self):
    mod = self
    for i,cax in enumerate(mod.labeled_conjs):
        fmla = cax.formula
        csname = 'conjecture:'+ str(i)
        variables = list(lu.used_variables_ast(fmla))   # ← list(set) — hash-bucket order
        sort = il.RelationSort([v.sort for v in variables])
        sym = il.Symbol(csname,sort)
        space = ics.NamedSpace(il.Literal(0,fmla))
        mod.concept_spaces.append((sym(*variables),space))
```

`lu.used_variables_ast = gen_to_set(variables_ast)` returns a **Python `set`** (`ivy_logic_utils.py:544`). `list(set)` iterates a set, which yields elements in CPython hash-bucket order (deterministic within one Python process given a fixed `PYTHONHASHSEED`, but not portable to Go).

**Go** (`/Users/jaten/ivy/goivy/module/module.go:830-874`, `UpdateConjs`):
```go
varMap := UsedVariablesAST(fmla)              // map[lg.NodeKey]lg.Expr
var variables []*lg.Variable
for _, v := range varMap {                    // ← Go map iteration — random order
    if vr, ok := v.(*lg.Variable); ok {
        variables = append(variables, vr)
    }
}
```

`UsedVariablesAST` returns a Go `map`. Go maps have intentionally randomized iteration order. So Go's order will not even match itself across runs.

Two unordered iterators producing two different orderings is the divergence.

### The fix: switch BOTH to traversal-order (left-to-right, first-occurrence dedup)

Python already exports an ordered variant: `used_variables_in_order_ast = gen_unique(variables_ast)` (`ivy_logic_utils.py:550`). It walks the AST left-to-right and yields each variable the first time it is seen — fully deterministic, no hashing involved.

Go already has the matching helper: `module.VariablesAST(node lg.Expr) []*lg.Variable` (`module/astutil.go:37`), implemented by `collectFreeVarsOrdered` (`module/astutil.go:324-369`), which is a faithful port of `variables_ast` + `gen_unique` (left-to-right DFS, dedup by name, skip bound variables).

Both helpers visit a `Variable` exactly when Python's `variables_ast` would yield it, and dedup the same way. The orderings will match by construction.

### Why this is safe

Concept-space labels are consumed by the alpha module / UI display loop and by `resort_concept_spaces` (`ivy_module.py:255`). The label is a function symbol applied to its free variables — the ordering of those variables determines the symbol's sort signature (`RelationSort`) but does not change the meaning of the conjecture itself.

For tlb.ivy, all label variables are `tlb.processor`, so `RelationSort([processor]*N)` is invariant under permutation. For other ivy programs with mixed-sort labels, the chosen order becomes the canonical order — the same on both sides. No verification logic depends on this order being any *particular* arbitrary value; it just needs to be consistent.

The original `list(set)` ordering in Python is already fragile (depends on `PYTHONHASHSEED`); replacing it with deterministic traversal order is a strict improvement.

## Fix

Two single-call edits — one Python, one Go.

### Edit 1: `/Users/jaten/ivy/pyivy/ivy/ivy/ivy_module.py:277`

```python
variables = list(lu.used_variables_ast(fmla))
```
→
```python
# Use deterministic left-to-right traversal order (first-occurrence dedup)
# instead of set hash-bucket order, so Go can produce the identical
# concept-space label ordering for cross-language canon comparison.
variables = list(lu.used_variables_in_order_ast(fmla))
```

`used_variables_in_order_ast` is already defined and used elsewhere (`ivy_logic_utils.py:1619`, `ivy_mc.py:1155`).

### Edit 2: `/Users/jaten/ivy/goivy/module/module.go:842-848`

Replace the map-iteration block with a call to the existing ordered helper:

```go
// Collect used variables from the formula.
varMap := UsedVariablesAST(fmla)
var variables []*lg.Variable
for _, v := range varMap {
    if vr, ok := v.(*lg.Variable); ok {
        variables = append(variables, vr)
    }
}
```
→
```go
// Collect free variables in left-to-right traversal order with
// first-occurrence dedup. Python uses lu.used_variables_in_order_ast
// in ivy_module.py:277 for the same reason: deterministic, portable
// ordering of concept-space label variables across languages.
variables := VariablesAST(fmla)
```

Update the doc comment on `UpdateConjs` to reflect the new Python source line:

```go
// Corresponds to Python Module.update_conjs (ivy_module.py:268-277):
//
//	for i,cax in enumerate(mod.labeled_conjs):
//	    fmla = cax.formula
//	    csname = 'conjecture:'+ str(i)
//	    variables = list(lu.used_variables_in_order_ast(fmla))
//	    sort = il.RelationSort([v.sort for v in variables])
//	    sym = il.Symbol(csname,sort)
//	    space = ics.NamedSpace(il.Literal(0,fmla))
//	    mod.concept_spaces.append((sym(*variables),space))
```

### What we are NOT changing

- Bodies of the labels (already match — same variables in the same nested positions).
- `_canon_concept_space_slice` / `canonConceptSpaceSlice` — they correctly delegate to `label.sexp()` and `body.sexp()`. The fix is at the construction site, not the canon site.
- Any other use of `used_variables_ast` (set version) elsewhere in Python — only the `update_conjs` call is affected by this divergence.
- `UsedVariablesAST` (the map version) in Go — still used by other callers that genuinely need set semantics.

## Verification

**Do NOT run `make tlb` from this agent — the user runs it on another machine.**

Local checks this agent will run after the user approves:

1. `go build ./module/... ./check/...` — confirm the change compiles.
2. `go test ./module/...` — confirm existing tests still pass. If a test was depending on the old map-iteration ordering, that test was relying on flaky Go behavior; investigate before changing the test.
3. Optional smoke check from Python side: `python3 -c "from ivy.ivy import ivy_logic_utils as lu; print(lu.used_variables_in_order_ast)"` to confirm the symbol is importable.

After the user re-runs `make tlb` on the other machine, expected outcomes:

A. **Best case**: xtrace 406841 (`conceptSpaces=[...]`) matches between Go and Python. The next divergence (if any) appears further along.

B. **Possible case**: ordering matches for labels but the BODIES diverge in some new way (e.g., a body uses `used_variables_ast` somewhere else). That would point to a sister site needing the same `_in_order_` fix.

C. **Unexpected case**: ordering still differs. Then either `VariablesAST` and `used_variables_in_order_ast` disagree on traversal (e.g., differing handling of `bound_variables`), or one of the conjecture formulas contains a node type one side recurses into and the other does not. Bisect by emitting the variable list before label construction on both sides.

## Critical files

- `/Users/jaten/ivy/pyivy/ivy/ivy/ivy_module.py` — Edit 1, line 277
- `/Users/jaten/ivy/goivy/module/module.go` — Edit 2, lines 820-874

## Reference (source of truth)

- `~/ivy/pyivy/ivy/ivy/ivy_logic_utils.py:523-535` — `variables_ast` generator (left-to-right DFS, skips bound variables)
- `~/ivy/pyivy/ivy/ivy/ivy_logic_utils.py:544` — `used_variables_ast = gen_to_set(variables_ast)` (set version — current divergent source)
- `~/ivy/pyivy/ivy/ivy/ivy_logic_utils.py:550` — `used_variables_in_order_ast = gen_unique(variables_ast)` (ordered version — the fix)
- `~/ivy/pyivy/ivy/ivy/ivy_utils.py:48-60` — `unique` / `gen_unique` (first-occurrence dedup)
- `~/ivy/goivy/module/astutil.go:37-49` — `VariablesAST` (Go's matching ordered helper)
- `~/ivy/goivy/module/astutil.go:324-369` — `collectFreeVarsOrdered` (DFS, skip bound, dedup by name)
- `~/ivy/goivy/module/module.go:830-874` — `UpdateConjs` (the call site to fix)

## What we are NOT investigating in this plan

- The original `invar386` TopSort panic. Each fix in this sequence moves the trace divergence further along; the panic, if still present, will surface once the trace is fully aligned.
- Other potential `used_variables_ast` (set) → `used_variables_in_order_ast` mismatches. Address them only if they show up as divergences in subsequent runs.
