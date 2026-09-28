package policy

import (
	"fmt"
	"strings"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// checkBucketAndCFS applies the generic gate every action shares: is the
// derived bucket in the action's AllowedBuckets, and (unless the action is
// exempt) is CFS connected (dev_docs/safety-architecture.md section 4.1,
// D5). set_light bypasses this entirely (checkSetLight has its own rule);
// every other action calls this first.
func checkBucketAndCFS(spec actionSpec, derived printerstate.Derived) *Error {
	if !spec.AllowedBuckets[derived.Bucket] {
		return &Error{
			Action: spec.Name,
			Code:   CodeUnavailable,
			Message: fmt.Sprintf("%s is not available in state %q (bucket %s): %s",
				spec.Name, derived.State, derived.Bucket, strings.Join(nonEmpty(derived.Reasons), "; ")),
		}
	}
	if spec.BlockedByCFS && derived.CFSConnected {
		return &Error{
			Action:  spec.Name,
			Code:    CodeUnavailable,
			Message: "CFS connected: control tools are disabled until v0.2.0 validates CFS state signals",
		}
	}
	return nil
}

func nonEmpty(reasons []string) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// checkAllowControl is applied before anything else in Execute: this whole
// package is the write path, so allow_control gates every action, including
// set_light.
func checkAllowControl(name ActionName, printer domain.Printer) *Error {
	if printer.AllowControl {
		return nil
	}
	return &Error{
		Action:  name,
		Code:    CodeForbidden,
		Message: "control is disabled for this printer (allow_control is false)",
	}
}

// proposalDiff reports which of a proposal's bound fields differ from a
// freshly read situation (dev_docs/safety-architecture.md section 3.3:
// "any bound field that differs invalidates the token"). An empty result
// means every bound field still matches.
func proposalDiff(p proposal, identity string, params Params, job *printerstate.JobIdentity, bucket printerstate.Bucket, class printerstate.GatingClass) []string {
	var changed []string
	if p.identity != identity {
		changed = append(changed, "printer")
	}
	if p.params != params {
		changed = append(changed, "params")
	}
	changed = append(changed, printerstate.JobIdentityDiff(p.job, job)...)
	if p.bucket != bucket {
		changed = append(changed, "bucket")
	}
	if p.class != class {
		changed = append(changed, "gating_class")
	}
	return changed
}

// findObject looks up name (case-insensitive, matching
// 10-hazard-analysis.md 2.5's confirmed upper-case storage) in an
// exclude_object.objects list, returning the exact stored name.
func findObject(objects []map[string]any, name string) (string, bool) {
	for _, obj := range objects {
		raw, ok := obj["name"]
		if !ok {
			continue
		}
		stored, ok := raw.(string)
		if !ok {
			continue
		}
		if strings.EqualFold(stored, name) {
			return stored, true
		}
	}
	return "", false
}

// isExcluded reports whether name (already the exact stored spelling) is in
// excluded.
func isExcluded(excluded []string, name string) bool {
	for _, e := range excluded {
		if strings.EqualFold(e, name) {
			return true
		}
	}
	return false
}

// liveTempCap builds a domain.LiveCap from a product_param reading. Known
// is false whenever the field was not reported, which is exactly the
// condition domain.EffectiveTemperatureLimit needs to refuse a write
// rather than silently fall back to the code ceiling alone (10-hazard-analysis.md
// section 6, item 5).
func liveTempCap(v *float64) domain.LiveCap {
	if v == nil {
		return domain.LiveCap{}
	}
	return domain.LiveCap{Value: *v, Known: true}
}
