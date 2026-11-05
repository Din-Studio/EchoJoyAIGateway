// Package configdependency describes broken references in a candidate configuration.
// It carries only public configuration identifiers, never raw configuration or secrets.
package configdependency

// Reason identifies an existing dependency check, independent of HTTP and locale.
type Reason string

const (
	JevRouteRequired         Reason = "jev_route_required"
	JevRouteUnavailable      Reason = "jev_route_unavailable"
	AutoModelTargetMissing   Reason = "auto_model_target_missing"
	AutoModelNameConflict    Reason = "auto_model_name_conflict"
	AutoModelFallbackMissing Reason = "auto_model_fallback_missing"
	AutoModelRecursiveTarget Reason = "auto_model_recursive_target"
)

// Error identifies the first broken reference found by the configuration compiler.
// Features contains auto_model and/or request_audit. GroupID is omitted for
// model-name-based routing; Model is the client-facing model name, not a secret.
type Error struct {
	Reason    Reason   `json:"reason"`
	Features  []string `json:"features"`
	GroupID   uint     `json:"group_id,omitempty"`
	Model     string   `json:"model,omitempty"`
	AutoModel string   `json:"auto_model,omitempty"`
	Preset    string   `json:"preset,omitempty"`
}

func (e *Error) Error() string { return "configuration dependency: " + string(e.Reason) }
