package agentdata

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Bound by Go to a frozen requirement and validated typed parameters. Model
// call IDs, purpose labels and candidate names are deliberately absent.
type WorkIdentity struct {
	RunID             string         `json:"run_id"`
	RequirementID     string         `json:"requirement_id"`
	ArtifactHash      string         `json:"artifact_hash"`
	Endpoint          string         `json:"endpoint"`
	Params            map[string]any `json:"params"`
	ContractVersion   string         `json:"contract_version"`
	ContractHash      string         `json:"contract_hash"`
	RefreshGeneration uint64         `json:"refresh_generation"`
}

func (w WorkIdentity) validate(units []string) error {
	if w.RunID == "" || w.ArtifactHash == "" || w.ContractVersion == "" || w.ContractHash == "" || w.RefreshGeneration == 0 {
		return fmt.Errorf("incomplete host work identity")
	}
	for _, unit := range units {
		if unit != "" && unit == w.RequirementID {
			return nil
		}
	}
	return fmt.Errorf("work requirement is not in the frozen contract")
}

func (w WorkIdentity) key(units []string, candidate bool) (string, error) {
	if err := w.validate(units); err != nil {
		return "", err
	}
	endpoint, err := CanonicalEndpoint(w.Endpoint)
	if err != nil {
		return "", err
	}
	params, err := CanonicalParameters(w.Params)
	if err != nil {
		return "", err
	}
	parts := []any{"work-v1", w.RunID, w.RequirementID, endpoint, params, w.ContractVersion, w.ContractHash}
	if !candidate {
		parts = append(parts, w.ArtifactHash, w.RefreshGeneration)
	} else {
		parts[0] = "adaptation-v1"
	}
	return Hash(parts), nil
}

func (w WorkIdentity) Key(frozenUnits []string) (string, error) { return w.key(frozenUnits, false) }

// A code hash or refresh cannot grant new candidate allowances for the same
// requirement, endpoint and parameters. Physical attempts use their own ordinal.
func (w WorkIdentity) AdaptationKey(frozenUnits []string) (string, error) {
	return w.key(frozenUnits, true)
}

func CanonicalEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("identity endpoint must be absolute HTTP(S)")
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", err
	}
	u.RawQuery = query.Encode() // Preserve values and repeated-value order, not just host/path.
	return u.String(), nil
}

type canonicalParameter struct {
	Kind  string `json:"kind"`
	Value any    `json:"value"`
}

func CanonicalParameters(params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	v, err := canonicalParameterValue(params)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	return raw, err
}

func canonicalParameterValue(value any) (canonicalParameter, error) {
	switch v := value.(type) {
	case nil:
		return canonicalParameter{Kind: "null"}, nil
	case string:
		return canonicalParameter{Kind: "string", Value: v}, nil
	case bool:
		return canonicalParameter{Kind: "boolean", Value: v}, nil
	case map[string]any:
		out := map[string]canonicalParameter{}
		for key, item := range v {
			normal, err := canonicalParameterValue(item)
			if err != nil {
				return canonicalParameter{}, err
			}
			out[key] = normal
		}
		return canonicalParameter{Kind: "object", Value: out}, nil
	case []any:
		out := make([]canonicalParameter, 0, len(v))
		for _, item := range v {
			normal, err := canonicalParameterValue(item)
			if err != nil {
				return canonicalParameter{}, err
			}
			out = append(out, normal)
		}
		return canonicalParameter{Kind: "array", Value: out}, nil
	case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32:
		raw, err := json.Marshal(value)
		if err != nil {
			return canonicalParameter{}, err
		}
		normal, err := canonicalDecimal(string(raw))
		if err != nil {
			return canonicalParameter{}, err
		}
		return canonicalParameter{Kind: "number", Value: normal}, nil
	default:
		// Programmatic callers may use typed JSON slices/maps. Round-trip once
		// through JSON without converting integers to float64.
		raw, err := json.Marshal(value)
		if err != nil {
			return canonicalParameter{}, err
		}
		var decoded any
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.UseNumber()
		if err = d.Decode(&decoded); err != nil {
			return canonicalParameter{}, err
		}
		return canonicalParameterValue(decoded)
	}
}

// Decimal coefficient/exponent form avoids allocating huge rational powers.
func canonicalDecimal(raw string) (string, error) {
	sign := ""
	if strings.HasPrefix(raw, "-") {
		sign = "-"
		raw = raw[1:]
	}
	coefficient, exponent := raw, int64(0)
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		coefficient = raw[:index]
		var err error
		exponent, err = strconv.ParseInt(raw[index+1:], 10, 64)
		if err != nil {
			return "", fmt.Errorf("numeric exponent outside supported identity range")
		}
	}
	fraction := 0
	if index := strings.IndexByte(coefficient, '.'); index >= 0 {
		fraction = len(coefficient) - index - 1
		coefficient = coefficient[:index] + coefficient[index+1:]
	}
	coefficient = strings.TrimLeft(coefficient, "0")
	if coefficient == "" {
		return "0", nil
	}
	trimmed := strings.TrimRight(coefficient, "0")
	delta := int64(len(coefficient) - len(trimmed) - fraction)
	if (delta > 0 && exponent > int64(^uint64(0)>>1)-delta) || (delta < 0 && exponent < -int64(^uint64(0)>>1)-1-delta) {
		return "", fmt.Errorf("numeric exponent outside supported identity range")
	}
	return sign + trimmed + "e" + strconv.FormatInt(exponent+delta, 10), nil
}

type FailureKind string

const (
	FailureInvalidProposal     FailureKind = "invalid_proposal"
	FailureCodeInvalid         FailureKind = "code_invalid"
	FailureNotStartedTransient FailureKind = "not_started_transient"
	FailureTransientExternal   FailureKind = "transient_external"
	FailurePermanentDenial     FailureKind = "permanent_denial"
	FailureUnknownOutcome      FailureKind = "outcome_unknown"
	FailureIncompleteDelivery  FailureKind = "incomplete_delivery"
	FailureLeaseLost           FailureKind = "lease_lost"
	FailureTargetSuperseded    FailureKind = "target_superseded"
)

func (k FailureKind) Validate() error {
	switch k {
	case FailureInvalidProposal, FailureCodeInvalid, FailureNotStartedTransient, FailureTransientExternal, FailurePermanentDenial, FailureUnknownOutcome, FailureIncompleteDelivery, FailureLeaseLost, FailureTargetSuperseded:
		return nil
	}
	return fmt.Errorf("unsupported failure kind %q", k)
}

type ExecutionStart string

const (
	StartNotStarted ExecutionStart = "not_started"
	StartStarted    ExecutionStart = "started"
	StartUnknown    ExecutionStart = "unknown"
)

type ExecutionOutcome string

const (
	OutcomeKnown   ExecutionOutcome = "known"
	OutcomeUnknown ExecutionOutcome = "unknown"
)

// Only the host executor may create this receipt. Unknown usage is never zero.
type ExecutionReceipt struct {
	WorkKey      string           `json:"work_key"`
	Attempt      uint32           `json:"attempt"`
	Start        ExecutionStart   `json:"start"`
	Outcome      ExecutionOutcome `json:"outcome"`
	UsageKnown   bool             `json:"usage_known"`
	HTTPRequests int64            `json:"http_requests"`
	HTTPBytes    int64            `json:"http_bytes"`
}

func (r ExecutionReceipt) Validate() error {
	if r.WorkKey == "" || r.Attempt == 0 || r.HTTPRequests < 0 || r.HTTPBytes < 0 {
		return fmt.Errorf("incomplete or negative execution receipt")
	}
	if r.Start != StartNotStarted && r.Start != StartStarted && r.Start != StartUnknown {
		return fmt.Errorf("unsupported execution start state")
	}
	if r.Outcome != OutcomeKnown && r.Outcome != OutcomeUnknown {
		return fmt.Errorf("unsupported execution outcome state")
	}
	if r.Start == StartNotStarted && (r.Outcome != OutcomeKnown || !r.UsageKnown || r.HTTPRequests != 0 || r.HTTPBytes != 0) {
		return fmt.Errorf("not_started requires a trusted known-zero receipt")
	}
	return nil
}
